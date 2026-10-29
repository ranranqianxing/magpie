package cursoride

import (
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// How long a trust-store probe may take, and how long an install that
// can show one OS prompt (Windows' root dialog, macOS' keychain) may wait.
const (
	trustProbe  = 20 * time.Second
	trustPrompt = 3 * time.Minute
)

// TrustResult is what checking, then maybe installing, the CA into the
// user trust store did. The machine-wide store is never written, and a
// connect that finds the CA already trusted does not install it again.
type TrustResult struct {
	// User is true when a store Cursor reads for this user now contains
	// the CA: Windows Current User Root, the macOS login keychain, or
	// (on Linux) the system bundle. The Linux NSS database is updated
	// when its certutil is installed; that alone does not clear Step,
	// because Cursor's systemCertificatesV2 reads the system bundle.
	User bool
	// Already is true when that store already held this CA, so no
	// install command was run. A connect that just added it has User
	// set and Already clear.
	Already bool
	// Step is the one action still left, or "" when nothing is.
	// TrustStepWindows, TrustStepDarwin, TrustStepLinux, TrustStepManual.
	Step string
	// Command is the one shell command for TrustStepLinux. It is not run.
	Command string
}

const (
	TrustStepWindows = "windows"
	TrustStepDarwin  = "darwin"
	TrustStepLinux   = "linux"
	TrustStepManual  = "manual"
)

// runTrust runs a trust-store tool. Tests replace it; production uses
// proc so a Windows build does not open a console for certutil.
var runTrust = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return proc.CommandContext(ctx, name, args...).CombinedOutput()
}

// trustHasCmd reports whether name is a program this machine can run.
// Tests replace it. update-ca-certificates lives in /usr/sbin, which a
// desktop launch often leaves off PATH.
var trustHasCmd = func(name string) bool {
	if p, err := exec.LookPath(name); err == nil && p != "" {
		return true
	}
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/bin", "/sbin"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// systemCertFiles are the bundles Node and Cursor read on Linux.
var systemCertFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/ssl/cert.pem",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
}

// TrustCA installs the PEM certificate at certPath into the user trust
// store. It does not call sudo, pkexec, or the machine store. Under
// MAGPIE_TEST_SANDBOX it installs nothing: a test must not touch the
// machine that runs it. Tests of the install itself call trustInstall
// with runTrust replaced.
func TrustCA(certPath string) TrustResult {
	if os.Getenv("MAGPIE_TEST_SANDBOX") != "" {
		return TrustResult{User: true}
	}
	return trustInstall(certPath)
}

func trustInstall(certPath string) TrustResult {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return trustFailed(certPath)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return trustFailed(certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return trustFailed(certPath)
	}
	sum := sha1.Sum(cert.Raw)
	fp := strings.ToUpper(hex.EncodeToString(sum[:]))
	switch runtime.GOOS {
	case "windows":
		return trustWindows(certPath, fp)
	case "darwin":
		return trustDarwin(certPath, fp)
	case "linux":
		return trustLinux(certPath, pemBytes, fp)
	default:
		return trustFailed(certPath)
	}
}

func trustFailed(certPath string) TrustResult {
	res := TrustResult{Step: trustStep(runtime.GOOS)}
	if res.Step == TrustStepLinux {
		res.Command = linuxTrustCommand(certPath)
	}
	return res
}

func trustStep(goos string) string {
	switch goos {
	case "windows":
		return TrustStepWindows
	case "darwin":
		return TrustStepDarwin
	case "linux":
		return TrustStepLinux
	default:
		return TrustStepManual
	}
}

func runOut(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runTrust(ctx, name, args...)
}

// trustWindows adds the CA to Current User\Root. certutil shows one
// consent dialog for that store; it does not ask for an administrator.
// The machine Root store is not used: that needs elevation.
func trustWindows(certPath, fp string) TrustResult {
	out, err := runOut(trustProbe, "certutil", "-user", "-verifystore", "Root", fp)
	if certutilHas(out, err) {
		return TrustResult{User: true, Already: true}
	}
	out, err = runOut(trustPrompt, "certutil", "-user", "-addstore", "-f", "Root", certPath)
	if err != nil || strings.Contains(strings.ToLower(string(out)), "failed") {
		log.Printf("cursor ide: user root store: %v", err)
		return TrustResult{Step: TrustStepWindows}
	}
	return TrustResult{User: true}
}

func certutilHas(out []byte, err error) bool {
	if err != nil {
		return false
	}
	s := strings.ToLower(string(out))
	if strings.Contains(s, "cannot find") || strings.Contains(s, "not found") || strings.Contains(s, "failed") {
		return false
	}
	return true
}

// trustDarwin adds the CA to the login keychain as a user trust root.
// add-trusted-cert without -d changes the user trust settings and asks
// for the login keychain password, not an administrator.
func trustDarwin(certPath, fp string) TrustResult {
	kc := loginKeychain()
	if kc == "" {
		return TrustResult{Step: TrustStepDarwin}
	}
	out, err := runOut(trustProbe, "security", "dump-trust-settings")
	if err == nil && darwinUserTrusted(out, fp) {
		return TrustResult{User: true, Already: true}
	}
	if _, err = runOut(trustPrompt, "security", "add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", kc, certPath); err != nil {
		log.Printf("cursor ide: login keychain: %v", err)
		return TrustResult{Step: TrustStepDarwin}
	}
	return TrustResult{User: true}
}

func loginKeychain() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	db := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	if st, err := os.Stat(db); err == nil && !st.IsDir() {
		return db
	}
	old := filepath.Join(home, "Library", "Keychains", "login.keychain")
	if st, err := os.Stat(old); err == nil && !st.IsDir() {
		return old
	}
	return db
}

func darwinUserTrusted(out []byte, fp string) bool {
	parts := strings.Split(string(out), "Cert ")
	want := strings.ToUpper(fp)
	for _, p := range parts[1:] {
		flat := strings.ToUpper(strings.ReplaceAll(p, " ", ""))
		if !strings.Contains(flat, want) && !strings.Contains(p, caCommonName) {
			continue
		}
		if strings.Contains(p, "TrustRoot") || strings.Contains(p, "TrustAsRoot") {
			return true
		}
	}
	return false
}

// trustLinux updates the user NSS database when certutil (libnss3-tools)
// is installed, which Chromium consults. Cursor's Node stack reads the
// system bundle instead, and writing that needs root, so the one command
// is returned and not run.
func trustLinux(certPath string, pemBytes []byte, _ string) TrustResult {
	if bundleHas(pemBytes) {
		return TrustResult{User: true, Already: true}
	}
	if trustHasCmd("certutil") {
		if err := nssTrust(certPath); err != nil {
			log.Printf("cursor ide: nss db: %v", err)
		}
	}
	return TrustResult{User: false, Step: TrustStepLinux, Command: linuxTrustCommand(certPath)}
}

func bundleHas(pemBytes []byte) bool {
	body := pemBody(pemBytes)
	if body == "" {
		return false
	}
	for _, path := range systemCertFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if bytesContainsPEM(b, body) {
			return true
		}
	}
	return false
}

func pemBody(pemBytes []byte) string {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ""
	}
	again := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	// Compare the base64, ignoring the header line and wrapping.
	s := string(again)
	s = strings.ReplaceAll(s, "-----BEGIN CERTIFICATE-----", "")
	s = strings.ReplaceAll(s, "-----END CERTIFICATE-----", "")
	return strings.Join(strings.Fields(s), "")
}

func bytesContainsPEM(bundle []byte, body string) bool {
	flat := strings.Join(strings.Fields(string(bundle)), "")
	return strings.Contains(flat, body)
}

func nssTrust(certPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if home == "" {
		return errf("no home for the NSS database")
	}
	db := filepath.Join(home, ".pki", "nssdb")
	if err := os.MkdirAll(db, 0o700); err != nil {
		return err
	}
	sql := "sql:" + db
	out, err := runOut(trustProbe, "certutil", "-d", sql, "-L")
	if err != nil {
		if err := nssInitDB(sql); err != nil {
			return err
		}
		out = nil
	}
	if nssHasCA(out) {
		return nil
	}
	_, _ = runOut(trustProbe, "certutil", "-d", sql, "-D", "-n", caCommonName)
	_, err = runOut(trustProbe, "certutil", "-d", sql, "-A", "-n", caCommonName, "-t", "C,,", "-i", certPath)
	return err
}

func nssInitDB(sql string) error {
	if _, err := runOut(trustProbe, "certutil", "-d", sql, "-N", "--empty-password"); err == nil {
		return nil
	}
	f, err := os.CreateTemp("", "magpie-nss-pw-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write([]byte("\n")); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = runOut(trustProbe, "certutil", "-d", sql, "-N", "-f", name)
	return err
}

func nssHasCA(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, caCommonName) && strings.Contains(line, "C,") {
			return true
		}
	}
	return false
}

func linuxTrustCommand(certPath string) string {
	q := shellQuote(certPath)
	switch {
	case trustHasCmd("update-ca-certificates"):
		return "sudo cp " + q + " /usr/local/share/ca-certificates/magpie-cursor-ide.crt && sudo update-ca-certificates"
	case trustHasCmd("update-ca-trust"):
		return "sudo cp " + q + " /etc/pki/ca-trust/source/anchors/magpie-cursor-ide.crt && sudo update-ca-trust extract"
	case trustHasCmd("trust"):
		return "sudo trust anchor --store " + q
	default:
		return "sudo cp " + q + " /usr/local/share/ca-certificates/magpie-cursor-ide.crt && sudo update-ca-certificates"
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
