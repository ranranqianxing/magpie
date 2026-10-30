package agent

// Cursor on the Agents page is the ordinary Cursor IDE. Cursor CLI
// (cursor in agents.go) only has Cursor's own model ids in
// cli-config.json, so that row cannot be connected. Cursor Private
// Inference (cursorlocal.go) reads CURSOR_LOCAL_AGENT_BASE_URL and never
// writes this IDE's settings. This row points the IDE's http.proxy at a
// proxy in this process. The proxy answers Cursor's model list with
// magpie's catalog and sends a magpie model run to the gateway. Cursor
// reads those settings when it starts.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/cursoride"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

// cursorIDEStashKey is the stash entry that holds Cursor's proxy settings
// from before magpie wrote its own, so disconnect puts them back.
const cursorIDEStashKey = "cursor-ide.proxy"

// The settings Cursor reads, as edit paths: a dot in the key is escaped
// because edit splits paths on dots. The gateway key is magpie's record of
// the address the proxy calls. Cursor ignores it. Sync's model list is the
// same kind of record; the proxy reads the catalog on each request.
const (
	cursorIDEProxyKey    = `http\.proxy`
	cursorIDEKerberosKey = `http\.proxyKerberosServicePrincipal`
	cursorIDESupportKey  = `http\.proxySupport`
	cursorIDEHTTP2Key    = `cursor\.general\.disableHttp2`
	cursorIDECertsKey    = `http\.experimental\.systemCertificatesV2`
	cursorIDENoProxyKey  = `http\.noProxy`
	cursorIDEGatewayKey  = `magpie\.cursorGateway`
	cursorIDEModelsKey   = `magpie\.cursorModels`
)

var cursorIDEManaged = []string{
	cursorIDEProxyKey, cursorIDEKerberosKey, cursorIDESupportKey,
	cursorIDEHTTP2Key, cursorIDECertsKey, cursorIDENoProxyKey,
	cursorIDEGatewayKey, cursorIDEModelsKey,
}

// cursorIDESaved is what settings.json held for the managed keys.
// Absent means the file was not there. A key missing from Raw was not in
// the file. A file that cannot be read is not stored as empty.
type cursorIDESaved struct {
	Absent bool              `json:"absent,omitempty"`
	Raw    map[string]string `json:"raw,omitempty"`
}

var (
	noticeCursorIDE              = newNotice("Cursor reads its proxy settings when it starts — quit and reopen Cursor to use magpie's models, and leave magpie running, then pick a magpie model in Cursor's model menu.")
	noticeCursorIDERestarted     = newNotice("Cursor was quit and opened again so it reads these proxy settings. Leave magpie running, then pick a magpie model in Cursor's model menu.")
	noticeCursorIDERestored      = newNotice("Cursor was quit and opened again so it reads the proxy settings that were put back.")
	noticeCursorIDERestoreManual = newNotice("Cursor reads its proxy settings when it starts — quit and reopen Cursor so the settings that were put back take effect.")
	noticeCursorIDEKept          = newNotice("magpie's certificate is already trusted for your user, so this connect did not install it again.")
	noticeCursorIDEWin           = newNotice("Approve the one Windows prompt to trust magpie's certificate for your user (it is not installed for the whole machine). If you dismissed it, disconnect and connect Cursor on the Agents page again, or open {cert} and install it under Current User → Trusted Root Certification Authorities.")
	noticeCursorIDEMac           = newNotice("Enter your login keychain password when macOS asks, to trust magpie's certificate for your user (no administrator password). If you dismissed it, disconnect and connect Cursor on the Agents page again, or open {cert} in Keychain Access and set When using this certificate to Always Trust.")
	noticeCursorIDELinux         = newNotice("Cursor on Linux also reads the system trust store, which still needs magpie's certificate. Run {command} once, then quit and reopen Cursor.")
	noticeCursorIDEManual        = newNotice("Trust magpie's certificate ({cert}) so Cursor accepts the proxy.")
)

// cursorIDETrustFn replaces TrustCA in tests. Production leaves it nil.
var cursorIDETrustFn func(certPath string) cursoride.TrustResult

var cursorIDETrustMu sync.Mutex
var cursorIDETrustRes cursoride.TrustResult

func cursorIDE(home string) *Agent {
	dir := cursorIDEUserDir(runtime.GOOS, home, appdir.Getenv)
	path := filepath.Join(dir, "settings.json")
	return &Agent{
		ID: cursoride.AgentID, Name: "Cursor", Icon: "cursor", Aliases: []string{"cursor-desktop"},
		Dir: dir, Path: path,
		// the IDE's model menu is the catalog picked for this row
		ListsModels: true,
		detect:      func() bool { return cursorIDEHere(dir) },
		Notice:      func() string { return cursorIDENotice(path) },
		OnKept:      cursorIDEOnKept,
		Check:       func() string { return cursorIDECheck(path) },
		Sync:        func() error { return cursorIDESync(path) },
		Fields: []Field{{
			Key: "provider", Label: "provider",
			Get: func() string {
				if cursorIDEProxySet(path) {
					return magpieID
				}
				return ""
			},
			Set: func(v string) error { return cursorIDESet(path, v) },
			Options: func(map[string]string) []Option {
				return []Option{{Value: magpieID, Label: "magpie", Icon: "magpie"}}
			},
		}},
	}
}

// cursorIDEUserDir is Cursor's User folder, where settings.json is.
// macOS uses Application Support, Windows %APPDATA%, Linux
// $XDG_CONFIG_HOME or ~/.config. The same places VS Code uses.
func cursorIDEUserDir(goos, home string, getenv func(string) string) string {
	var root string
	switch goos {
	case "darwin":
		root = filepath.Join(home, "Library", "Application Support")
	case "windows":
		root = getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
	default:
		root = getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(root, "Cursor", "User")
}

func cursorIDECADir() string { return filepath.Join(appdir.Config(), "cursor-ide") }

func cursorIDEOnKept() error {
	if err := cursorIDEEnsure(); err != nil {
		return err
	}
	cursorIDEApplyTrust()
	return nil
}

func cursorIDEApplyTrust() {
	cert := cursoride.CertFile(cursorIDECADir())
	var res cursoride.TrustResult
	if cursorIDETrustFn != nil {
		res = cursorIDETrustFn(cert)
	} else {
		res = cursoride.TrustCA(cert)
	}
	cursorIDETrustMu.Lock()
	cursorIDETrustRes = res
	cursorIDETrustMu.Unlock()
	// A trust failure is recorded for the notice. It does not fail connect.
	switch {
	case res.Already:
		log.Printf("Cursor: certificate is already trusted for this user; install skipped")
	case res.User && res.Step == "":
		log.Printf("Cursor: certificate installed for this user")
	case res.Step != "":
		log.Printf("Cursor: certificate is not in the user trust store yet (%s)", res.Step)
	}
}

func cursorIDENotice(settingsPath string) string {
	cursorIDETrustMu.Lock()
	res := cursorIDETrustRes
	restarted := cursorIDERestarted
	restored := cursorIDERestored
	cursorIDETrustMu.Unlock()
	if restored {
		if restarted {
			return noticeCursorIDERestored.String()
		}
		return noticeCursorIDERestoreManual.String()
	}
	base := noticeCursorIDE.String()
	if restarted && cursorIDEProxySet(settingsPath) {
		base = noticeCursorIDERestarted.String()
	}
	if !cursorIDEProxySet(settingsPath) {
		return noticeCursorIDE.String()
	}
	cert := cursoride.CertFile(cursorIDECADir())
	var extra string
	switch res.Step {
	case cursoride.TrustStepWindows:
		extra = noticeCursorIDEWin.say("cert", cert)
	case cursoride.TrustStepDarwin:
		extra = noticeCursorIDEMac.say("cert", cert)
	case cursoride.TrustStepLinux:
		if res.Command == "" {
			extra = noticeCursorIDEManual.say("cert", cert)
		} else {
			extra = noticeCursorIDELinux.say("command", res.Command)
		}
	case cursoride.TrustStepManual:
		extra = noticeCursorIDEManual.say("cert", cert)
	}
	if extra == "" && res.Already {
		extra = noticeCursorIDEKept.String()
	}
	if extra == "" {
		return base
	}
	return base + " " + extra
}

func cursorIDEProxySet(path string) bool {
	url := cursoride.ProxyURL()
	want := []struct{ key, v string }{
		{cursorIDEProxyKey, url},
		{cursorIDEKerberosKey, url},
		{cursorIDESupportKey, "on"},
		{cursorIDEHTTP2Key, "true"},
		{cursorIDECertsKey, "true"},
	}
	for _, w := range want {
		v, ok := edit.GetJSON(path, w.key)
		if !ok || v != w.v {
			return false
		}
	}
	return true
}

func cursorIDEMatches(path string) bool {
	if !cursorIDEProxySet(path) {
		return false
	}
	if _, ok := edit.GetJSON(path, cursorIDENoProxyKey); ok {
		return false
	}
	g, ok := edit.GetJSON(path, cursorIDEGatewayKey)
	return ok && g == gateway.URL()
}

func cursorIDECheck(path string) string {
	if !cursorIDEProxySet(path) {
		return ""
	}
	if _, ok := edit.GetJSON(path, cursorIDENoProxyKey); ok {
		return "Cursor's http.noProxy (settings.json) is set, so Cursor can bypass magpie's proxy"
	}
	return wiringOff("Cursor", path, func(k string) (string, bool) {
		return edit.GetJSON(path, k)
	}, cursorIDEGatewayKey, gateway.URL())
}

func cursorIDESet(path, v string) error {
	switch v {
	case magpieID:
		if cursorIDEMatches(path) {
			if err := cursorIDEEnsure(); err != nil {
				return err
			}
			cursorIDEApplyTrust()
			cursorIDETrustMu.Lock()
			cursorIDERestored = false
			cursorIDETrustMu.Unlock()
			return nil
		}
		first := stashLoad()[cursorIDEStashKey] == ""
		if err := cursorIDERemember(path); err != nil {
			return err
		}
		if err := cursorIDEEnsure(); err != nil {
			if first {
				forget(cursorIDEStashKey)
			}
			return err
		}
		if err := cursorIDEWrite(path); err != nil {
			if first {
				forget(cursorIDEStashKey)
			}
			return err
		}
		cursorIDEApplyTrust()
		cursorIDETrustMu.Lock()
		cursorIDERestored = false
		cursorIDETrustMu.Unlock()
		cursorIDEAfterWrite()
		return nil
	case "":
		// Disconnect puts Cursor's settings back and stops the proxy.
		// The CA stays trusted: the next connect checks and skips the
		// install when it is still there. A restore that puts the
		// previous settings back quits Cursor and opens it again, the
		// same way a connect that wrote settings does. A disconnect
		// that leaves the file alone does not.
		restored, err := cursorIDERestore(path)
		if err != nil {
			return err
		}
		cursorIDEStop()
		cursorIDETrustMu.Lock()
		cursorIDERestored = restored
		if !restored {
			cursorIDERestarted = false
		}
		cursorIDETrustMu.Unlock()
		if restored {
			cursorIDEAfterWrite()
		}
		return nil
	default:
		return fmt.Errorf("cursor: provider %q", v)
	}
}

func cursorIDEWrite(path string) error {
	url := cursoride.ProxyURL()
	if err := edit.SetJSON(path,
		edit.KV{Path: cursorIDEProxyKey, Value: url},
		edit.KV{Path: cursorIDEKerberosKey, Value: url},
		edit.KV{Path: cursorIDESupportKey, Value: "on"},
		edit.KV{Path: cursorIDEHTTP2Key, Value: true},
		edit.KV{Path: cursorIDECertsKey, Value: true},
		edit.KV{Path: cursorIDEGatewayKey, Value: gateway.URL()},
	); err != nil {
		return err
	}
	return edit.DelJSON(path, cursorIDENoProxyKey)
}

func cursorIDERemember(path string) error {
	if _, ok := stashLoad()[cursorIDEStashKey]; ok {
		return nil
	}
	saved := cursorIDESaved{Raw: map[string]string{}}
	st, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		saved.Absent = true
	case err != nil:
		return err
	case st.IsDir():
		return fmt.Errorf("cursor: %s is a directory", path)
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) > 0 {
			for _, k := range cursorIDEManaged {
				raw, ok := edit.GetJSONRaw(path, k)
				if ok {
					saved.Raw[k] = raw
				}
			}
		}
	}
	body, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	stash(map[string]string{cursorIDEStashKey: string(body)})
	return nil
}

// cursorIDERestore puts the stashed proxy settings back. False means
// the file was left alone: nothing was connected, and these are not
// magpie's proxy settings, so a disconnect must not delete a proxy the
// user set and must not quit Cursor. True means the restore ran, which
// is when a connect would have relaunched after its own write.
func cursorIDERestore(path string) (bool, error) {
	raw := unstash(cursorIDEStashKey)
	if raw == "" && !cursorIDEProxySet(path) {
		return false, nil
	}
	var saved cursorIDESaved
	if raw != "" && json.Unmarshal([]byte(raw), &saved) != nil {
		return false, fmt.Errorf("cursor: saved proxy settings are not readable")
	}
	var del []string
	for _, k := range cursorIDEManaged {
		if _, ok := saved.Raw[k]; !ok {
			del = append(del, k)
		}
	}
	if err := edit.DelJSON(path, del...); err != nil {
		return false, err
	}
	var put []edit.KV
	for k, v := range saved.Raw {
		put = append(put, edit.KV{Path: k, Value: json.RawMessage(v)})
	}
	if len(put) > 0 {
		if err := edit.SetJSON(path, put...); err != nil {
			return false, err
		}
	}
	if !saved.Absent {
		return true, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "{}" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}

func cursorIDESync(path string) error {
	if !cursorIDEProxySet(path) {
		return nil
	}
	var ids []string
	shown, _ := provider.CatalogFor(cursoride.AgentID)
	for _, e := range shown {
		if id := strings.TrimSpace(e.ID); id != "" {
			ids = append(ids, id)
		}
	}
	raw, ok := edit.GetJSONRaw(path, cursorIDEModelsKey)
	if ok {
		var got []string
		if json.Unmarshal([]byte(raw), &got) == nil && slices.Equal(got, ids) {
			return nil
		}
	}
	return edit.SetJSON(path, edit.KV{Path: cursorIDEModelsKey, Value: ids})
}

// The proxy is one for the process. Connect starts it. Disconnect stops
// it. A later connect starts it again.
var cursorIDEProxy struct {
	mu sync.Mutex
	p  *cursoride.Proxy
}

func cursorIDEEnsure() error {
	cursorIDEProxy.mu.Lock()
	defer cursorIDEProxy.mu.Unlock()
	if cursorIDEProxy.p != nil {
		_, err := cursorIDEProxy.p.Start()
		return err
	}
	p := &cursoride.Proxy{
		Addr:  cursoride.ListenAddr,
		CADir: cursorIDECADir(),
		Gateway: func() (string, string) {
			return gateway.URL(), gateway.TokenFor(cursoride.AgentID)
		},
		Models: func() []cursoride.Model { return cursoride.ForAgent(cursoride.AgentID) },
	}
	if _, err := p.Start(); err != nil {
		return err
	}
	cursorIDEProxy.p = p
	return nil
}

func cursorIDEStop() {
	cursorIDEProxy.mu.Lock()
	p := cursorIDEProxy.p
	cursorIDEProxy.p = nil
	cursorIDEProxy.mu.Unlock()
	if p != nil {
		_ = p.Close()
	}
}

// KeepCursorIDE starts the proxy again when magpie serves and Cursor is
// already connected. A restart of magpie would otherwise leave Cursor
// pointed at a proxy that is not listening. It does not write settings.
func KeepCursorIDE(context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(cursorIDEUserDir(runtime.GOOS, home, appdir.Getenv), "settings.json")
	if !cursorIDEMatches(path) {
		return
	}
	if err := cursorIDEEnsure(); err != nil {
		log.Printf("Cursor: proxy: %v", err)
	}
}

// cursorIDEHere reports an ordinary Cursor install, or a Cursor User
// folder on a machine that does not have Cursor Private Inference. That
// build shares this User folder, and its row is the environment one.
func cursorIDEHere(userDir string) bool {
	if cursorIDELook() != "" {
		return true
	}
	if !isDir(userDir) {
		return false
	}
	return cursorLocalApp() == ""
}

// cursorIDELook is where ordinary Cursor is installed, "" when it is not.
// A var so a test can say, without a scan of the machine.
var cursorIDELook = cursorIDEFind

var cursorIDESeen struct {
	sync.Mutex
	app string
	at  time.Time
}

func cursorIDEFind() string {
	cursorIDESeen.Lock()
	defer cursorIDESeen.Unlock()
	if time.Since(cursorIDESeen.at) < 30*time.Second && !cursorIDESeen.at.IsZero() {
		return cursorIDESeen.app
	}
	cursorIDESeen.app = findCursorIDE(cursorLocalRoots())
	cursorIDESeen.at = time.Now()
	return cursorIDESeen.app
}

func findCursorIDE(roots []string) string {
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if exe := cursorIDEAt(filepath.Join(root, e.Name())); exe != "" {
				return exe
			}
		}
	}
	return ""
}

func cursorIDEAt(dir string) string {
	name := filepath.Base(dir)
	if strings.HasSuffix(name, ".app") {
		res := filepath.Join(dir, "Contents", "Resources", "app")
		if p, ok := cursorIDEProduct(res); ok {
			if exe := plistExecutable(filepath.Join(dir, "Contents", "Info.plist")); exe != "" {
				return filepath.Join(dir, "Contents", "MacOS", exe)
			}
			return filepath.Join(dir, "Contents", "MacOS", p.macExe())
		}
		return ""
	}
	if cursorIDEAppImage(name) && isFile(dir) {
		return dir
	}
	if p, ok := cursorIDEProduct(filepath.Join(dir, "resources", "app")); ok {
		exe := p.exe()
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		return filepath.Join(dir, exe)
	}
	return ""
}

// cursorIDEAppImage is a regular Cursor AppImage. Cursor Private
// Inference's download name is not one.
func cursorIDEAppImage(name string) bool {
	n := strings.ToLower(name)
	if !strings.HasSuffix(n, ".appimage") || cursorLocalAppImage(name) {
		return false
	}
	base := strings.TrimSuffix(n, ".appimage")
	return base == "cursor" || strings.HasPrefix(base, "cursor-")
}

func cursorIDEProduct(res string) (cursorProduct, bool) {
	var p cursorProduct
	b, err := os.ReadFile(filepath.Join(res, "product.json"))
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, strings.EqualFold(strings.TrimSpace(p.NameShort), "Cursor")
}
