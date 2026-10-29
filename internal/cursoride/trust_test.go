package cursoride

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withTrustRun(t *testing.T, fn func(name string, args []string) ([]byte, error)) *[]string {
	t.Helper()
	var got []string
	prev := runTrust
	runTrust = func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		got = append(got, line)
		if fn == nil {
			return nil, nil
		}
		return fn(name, args)
	}
	t.Cleanup(func() { runTrust = prev })
	return &got
}

func testCert(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	return CertFile(dir)
}

func TestTrustCADoesNotRunUnderTheTestSandbox(t *testing.T) {
	if os.Getenv("MAGPIE_TEST_SANDBOX") == "" {
		t.Fatal("testenv did not set MAGPIE_TEST_SANDBOX")
	}
	got := withTrustRun(t, func(string, []string) ([]byte, error) {
		t.Fatal("TrustCA ran a command")
		return nil, nil
	})
	res := TrustCA(testCert(t))
	if !res.User || res.Step != "" || len(*got) != 0 {
		t.Fatalf("result %+v calls %v", res, *got)
	}
}

func spacedHash(fp string) string {
	var b strings.Builder
	fp = strings.ToUpper(fp)
	for i := 0; i < len(fp); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fp[i : i+2])
	}
	return b.String()
}

func TestTrustWindowsUsesTheUserRootStore(t *testing.T) {
	cert := testCert(t)
	const fp = "00112233445566778899AABBCCDDEEFF00112233"
	const stale = "AABBCCDDEEFF00112233445566778899AABBCCDD"
	t.Run("already there", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if name != "certutil" || !strings.Contains(joined, "-store") || strings.Contains(joined, "-addstore") || strings.Contains(joined, "-verifystore") {
				t.Fatalf("probe %s %v", name, args)
			}
			return []byte("================ Certificate 0 ================\nSubject: CN=" + caCommonName + "\nCert Hash(sha1): " + spacedHash(fp) + "\n"), nil
		})
		res := trustWindows(cert, fp, true)
		if !res.User || !res.Already || res.Step != "" || len(*got) != 1 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a localized store listing still counts as this certificate", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "-addstore") {
				t.Fatal("addstore ran for a certificate already in the store")
			}
			return []byte("================ 证书 0 ================\n使用者: CN=" + caCommonName + ", O=magpie\n证书哈希(sha1): " + strings.ToLower(spacedHash(fp)) + "\n"), nil
		})
		res := trustWindows(cert, fp, true)
		if !res.User || !res.Already || len(*got) != 1 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("install prompts in the user store", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if name != "certutil" {
				t.Fatalf("command %s", name)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-user") || strings.Contains(joined, "LocalMachine") {
				t.Fatalf("store %s", joined)
			}
			if strings.Contains(joined, "-store") && !strings.Contains(joined, "-addstore") && !strings.Contains(joined, "-delstore") {
				return []byte("================ Certificate 0 ================\nSubject: CN=Some Other CA\nCert Hash(sha1): " + spacedHash(stale) + "\n"), nil
			}
			if !strings.Contains(joined, "-addstore") || !strings.Contains(joined, "Root") || !strings.Contains(joined, cert) {
				t.Fatalf("addstore %s", joined)
			}
			return []byte("CertUtil: -addstore command completed successfully."), nil
		})
		res := trustWindows(cert, fp, true)
		if !res.User || res.Already || res.Step != "" || len(*got) != 2 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a stale magpie CA is removed before this file is installed", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "-store") && !strings.Contains(joined, "-addstore") && !strings.Contains(joined, "-delstore"):
				return []byte("================ Certificate 0 ================\nSubject: CN=" + caCommonName + "\nCert Hash(sha1): " + spacedHash(stale) + "\n================ Certificate 1 ================\nSubject: CN=Other\nCert Hash(sha1): " + spacedHash("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF") + "\n"), nil
			case strings.Contains(joined, "-delstore"):
				if !strings.Contains(strings.ToUpper(joined), stale) {
					t.Fatalf("deleted %s", joined)
				}
				if strings.Contains(strings.ToUpper(joined), fp) {
					t.Fatalf("deleted the current certificate %s", joined)
				}
				return []byte("CertUtil: -delstore command completed successfully."), nil
			case strings.Contains(joined, "-addstore") && strings.Contains(joined, cert):
				return []byte("CertUtil: -addstore command completed successfully."), nil
			default:
				t.Fatalf("unexpected %s", joined)
				return nil, nil
			}
		})
		res := trustWindows(cert, fp, true)
		if !res.User || res.Already || res.Step != "" {
			t.Fatalf("%+v calls %v", res, *got)
		}
		joined := strings.Join(*got, "\n")
		if !strings.Contains(joined, "-delstore") || !strings.Contains(joined, "-addstore") {
			t.Fatalf("calls %v", *got)
		}
		if strings.Contains(strings.ToUpper(joined), "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF") {
			t.Fatalf("deleted a certificate that is not magpie's: %v", *got)
		}
	})
	t.Run("an expired file is not left as already trusted", func(t *testing.T) {
		got := withTrustRun(t, func(_ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "-addstore") {
				t.Fatal("an expired certificate was installed")
			}
			if strings.Contains(joined, "-delstore") {
				if !strings.Contains(strings.ToUpper(joined), fp) {
					t.Fatalf("deleted %s", joined)
				}
				return []byte("ok"), nil
			}
			return []byte("================ Certificate 0 ================\nSubject: CN=" + caCommonName + "\nCert Hash(sha1): " + spacedHash(fp) + "\n"), nil
		})
		res := trustWindows(cert, fp, false)
		if res.Already || res.User || res.Step != TrustStepWindows {
			t.Fatalf("%+v calls %v", res, *got)
		}
		if !strings.Contains(strings.Join(*got, " "), "-delstore") {
			t.Fatalf("calls %v", *got)
		}
	})
	t.Run("a declined prompt is the one windows step", func(t *testing.T) {
		withTrustRun(t, func(_ string, args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "-addstore") {
				return nil, errors.New("cancelled")
			}
			return []byte(""), nil
		})
		res := trustWindows(cert, fp, true)
		if res.User || res.Step != TrustStepWindows {
			t.Fatalf("%+v", res)
		}
	})
}

func TestTrustDarwinUsesTheLoginKeychain(t *testing.T) {
	cert := testCert(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(filepath.Dir(kc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte("not a keychain"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := "00112233445566778899AABBCCDDEEFF00112233"
	t.Run("already trusted", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if name != "security" || len(args) < 1 || args[0] != "dump-trust-settings" {
				t.Fatalf("%s %v", name, args)
			}
			return []byte("Cert 0: " + caCommonName + "\n   SHA-1 hash: " + fp + "\n   Result Type : kSecTrustSettingsResultTrustRoot\n"), nil
		})
		res := trustDarwin(cert, fp, true)
		if !res.User || !res.Already || res.Step != "" || len(*got) != 1 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("add is user trust not the system keychain", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "dump-trust-settings") {
				return []byte("No Trust Settings were found"), nil
			}
			if name != "security" || !strings.Contains(joined, "add-trusted-cert") {
				t.Fatalf("%s %s", name, joined)
			}
			if strings.Contains(joined, " -d") || strings.Contains(joined, "System.keychain") || !strings.Contains(joined, kc) {
				t.Fatalf("keychain %s", joined)
			}
			if !strings.Contains(joined, "-r trustRoot") || !strings.Contains(joined, cert) {
				t.Fatalf("trust %s", joined)
			}
			return nil, nil
		})
		res := trustDarwin(cert, fp, true)
		if !res.User || res.Already || res.Step != "" || len(*got) != 2 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a different magpie certificate is replaced", func(t *testing.T) {
		const old = "AABBCCDDEEFF00112233445566778899AABBCCDD"
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "dump-trust-settings"):
				return []byte("Cert 0: " + caCommonName + "\n   SHA-1 hash: " + old + "\n   Result Type : kSecTrustSettingsResultTrustRoot\n"), nil
			case strings.Contains(joined, "delete-certificate"):
				if !strings.Contains(joined, old) || !strings.Contains(joined, "-t") {
					t.Fatalf("delete %s", joined)
				}
				return nil, nil
			case strings.Contains(joined, "add-trusted-cert") && strings.Contains(joined, cert):
				return nil, nil
			default:
				t.Fatalf("unexpected %s %s", name, joined)
				return nil, nil
			}
		})
		res := trustDarwin(cert, fp, true)
		if !res.User || res.Already || res.Step != "" {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("an expired file is not treated as already trusted", func(t *testing.T) {
		got := withTrustRun(t, func(_ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "add-trusted-cert") {
				t.Fatal("an expired certificate was installed")
			}
			if strings.Contains(joined, "delete-certificate") {
				if !strings.Contains(joined, fp) {
					t.Fatalf("delete %s", joined)
				}
				return nil, nil
			}
			return []byte("Cert 0: " + caCommonName + "\n   SHA-1 hash: " + fp + "\n   TrustRoot\n"), nil
		})
		res := trustDarwin(cert, fp, false)
		if res.Already || res.User || res.Step != TrustStepDarwin {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a declined prompt is the one mac step", func(t *testing.T) {
		withTrustRun(t, func(_ string, args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "dump-trust-settings") {
				return nil, errors.New("no settings")
			}
			return nil, errors.New("user canceled")
		})
		res := trustDarwin(cert, fp, true)
		if res.User || res.Step != TrustStepDarwin {
			t.Fatalf("%+v", res)
		}
	})
}

func TestTrustLinuxUpdatesNSSAndLeavesTheSystemStore(t *testing.T) {
	cert := testCert(t)
	pemBytes, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	prevFiles, prevHas := systemCertFiles, trustHasCmd
	t.Cleanup(func() {
		systemCertFiles = prevFiles
		trustHasCmd = prevHas
	})
	empty := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(empty, []byte("not our ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	systemCertFiles = []string{empty}

	t.Run("system bundle already has it", func(t *testing.T) {
		bundle := filepath.Join(t.TempDir(), "ca-certificates.crt")
		if err := os.WriteFile(bundle, pemBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		systemCertFiles = []string{bundle}
		got := withTrustRun(t, func(string, []string) ([]byte, error) {
			t.Fatal("ran a command")
			return nil, nil
		})
		res := trustLinux(cert, pemBytes, "ABCDEF", true)
		if !res.User || !res.Already || res.Step != "" || len(*got) != 0 {
			t.Fatalf("%+v calls %v", res, *got)
		}
		systemCertFiles = []string{empty}
	})
	t.Run("an expired copy in the system bundle is not already trusted", func(t *testing.T) {
		bundle := filepath.Join(t.TempDir(), "ca-certificates.crt")
		if err := os.WriteFile(bundle, pemBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		systemCertFiles = []string{bundle}
		trustHasCmd = func(string) bool { return false }
		res := trustLinux(cert, pemBytes, "ABCDEF", false)
		if res.Already || res.Step != TrustStepLinux {
			t.Fatalf("%+v", res)
		}
		systemCertFiles = []string{empty}
	})
	t.Run("nss is updated and the system command is not run", func(t *testing.T) {
		trustHasCmd = func(name string) bool { return name == "certutil" || name == "update-ca-certificates" }
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if name != "certutil" {
				t.Fatalf("ran %s", name)
			}
			joined := strings.Join(args, " ")
			if strings.Contains(joined, " -a") {
				return pemBytes, nil
			}
			if strings.Contains(joined, " -L") {
				return []byte(caCommonName + "                                         C,,\n"), nil
			}
			t.Fatalf("unexpected %s", joined)
			return nil, nil
		})
		res := trustLinux(cert, pemBytes, "ABCDEF", true)
		if res.User || res.Step != TrustStepLinux {
			t.Fatalf("%+v", res)
		}
		if !strings.Contains(res.Command, "sudo update-ca-certificates") || !strings.Contains(res.Command, cert) {
			t.Fatalf("command %q", res.Command)
		}
		for _, line := range *got {
			if strings.Contains(line, "sudo") || strings.Contains(line, "update-ca") {
				t.Fatalf("ran %s", line)
			}
		}
	})
	t.Run("a different NSS certificate is replaced", func(t *testing.T) {
		trustHasCmd = func(name string) bool { return name == "certutil" || name == "update-ca-certificates" }
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, " -a"):
				return []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), nil
			case strings.Contains(joined, " -L"):
				return []byte(caCommonName + "                                         C,,\n"), nil
			case strings.Contains(joined, " -D "), strings.Contains(joined, " -A "):
				return nil, nil
			default:
				t.Fatalf("unexpected %s", joined)
				return nil, nil
			}
		})
		res := trustLinux(cert, pemBytes, "ABCDEF", true)
		if res.Step != TrustStepLinux {
			t.Fatalf("%+v", res)
		}
		joined := strings.Join(*got, "\n")
		if !strings.Contains(joined, " -D ") || !strings.Contains(joined, " -A ") {
			t.Fatalf("a different NSS certificate was left in place: %v", *got)
		}
		for _, line := range *got {
			if strings.Contains(line, "sudo") || strings.Contains(line, "update-ca") {
				t.Fatalf("ran %s", line)
			}
		}
	})
	t.Run("missing certutil still names one system command", func(t *testing.T) {
		trustHasCmd = func(name string) bool { return name == "update-ca-trust" }
		got := withTrustRun(t, nil)
		res := trustLinux(cert, pemBytes, "ABCDEF", true)
		if res.Step != TrustStepLinux || !strings.Contains(res.Command, "update-ca-trust extract") || len(*got) != 0 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a new nss database is created without a password prompt", func(t *testing.T) {
		trustHasCmd = func(name string) bool { return name == "certutil" || name == "update-ca-certificates" }
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, " -L"):
				return nil, errors.New("no db")
			case strings.Contains(joined, "--empty-password"):
				return nil, nil
			case strings.Contains(joined, " -D "):
				return nil, errors.New("not found")
			case strings.Contains(joined, " -A ") && strings.Contains(joined, "-t C,,") && strings.Contains(joined, cert):
				return nil, nil
			default:
				t.Fatalf("unexpected %s %s", name, joined)
				return nil, nil
			}
		})
		res := trustLinux(cert, pemBytes, "ABCDEF", true)
		if res.Step != TrustStepLinux || len(*got) < 3 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
}

func TestLinuxTrustCommandQuotesTheCertPath(t *testing.T) {
	prev := trustHasCmd
	t.Cleanup(func() { trustHasCmd = prev })
	trustHasCmd = func(name string) bool { return name == "update-ca-certificates" }
	cmd := linuxTrustCommand("/home/o'brien/ca.crt")
	if !strings.Contains(cmd, `'/home/o'\''brien/ca.crt'`) {
		t.Fatalf("command %q", cmd)
	}
}
