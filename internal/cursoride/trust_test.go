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

func TestTrustWindowsUsesTheUserRootStore(t *testing.T) {
	cert := testCert(t)
	t.Run("already there", func(t *testing.T) {
		got := withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if name != "certutil" || !strings.Contains(strings.Join(args, " "), "-verifystore") {
				t.Fatalf("probe %s %v", name, args)
			}
			return []byte("================ Certificate 0 ================\n"), nil
		})
		res := trustWindows(cert, "ABCDEF")
		if !res.User || !res.Already || res.Step != "" || len(*got) != 1 {
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
			if strings.Contains(joined, "-verifystore") {
				return nil, errors.New("cannot find the certificate")
			}
			if !strings.Contains(joined, "-addstore") || !strings.Contains(joined, "Root") || !strings.Contains(joined, cert) {
				t.Fatalf("addstore %s", joined)
			}
			return []byte("CertUtil: -addstore command completed successfully."), nil
		})
		res := trustWindows(cert, "ABCDEF")
		if !res.User || res.Already || res.Step != "" || len(*got) != 2 {
			t.Fatalf("%+v calls %v", res, *got)
		}
	})
	t.Run("a declined prompt is the one windows step", func(t *testing.T) {
		withTrustRun(t, func(name string, args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "-verifystore") {
				return nil, errors.New("cannot find")
			}
			return nil, errors.New("cancelled")
		})
		res := trustWindows(cert, "ABCDEF")
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
		res := trustDarwin(cert, fp)
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
		res := trustDarwin(cert, fp)
		if !res.User || res.Already || res.Step != "" || len(*got) != 2 {
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
		res := trustDarwin(cert, fp)
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
		res := trustLinux(cert, pemBytes, "ABCDEF")
		if !res.User || !res.Already || res.Step != "" || len(*got) != 0 {
			t.Fatalf("%+v calls %v", res, *got)
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
			if strings.Contains(joined, " -L") {
				return []byte(caCommonName + "                                         C,,\n"), nil
			}
			t.Fatalf("unexpected %s", joined)
			return nil, nil
		})
		res := trustLinux(cert, pemBytes, "ABCDEF")
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
	t.Run("missing certutil still names one system command", func(t *testing.T) {
		trustHasCmd = func(name string) bool { return name == "update-ca-trust" }
		got := withTrustRun(t, nil)
		res := trustLinux(cert, pemBytes, "ABCDEF")
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
		res := trustLinux(cert, pemBytes, "ABCDEF")
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
