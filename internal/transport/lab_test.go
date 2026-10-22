package transport

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNATLab runs real nodes through two NATs and a relay built from network
// namespaces inside an unprivileged user namespace (testdata/natlab.sh), and
// checks that each scenario lands on the expected path.
func TestNATLab(t *testing.T) {
	if testing.Short() {
		t.Skip("NAT lab skipped in -short mode")
	}
	if err := exec.Command("unshare", "--user", "--map-root-user", "--net", "--mount", "true").Run(); err != nil {
		t.Skipf("no unprivileged user namespaces: %v", err)
	}
	for _, tool := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "labnode")
	if out, err := exec.Command("go", "build", "-o", bin, "./labnode").CombinedOutput(); err != nil {
		t.Fatalf("build labnode: %v\n%s", err, out)
	}
	out := filepath.Join(dir, "out")
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--net", "--mount", "testdata/natlab.sh", bin, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("natlab.sh: %v\n%s", err, b)
	}

	want := map[string]string{
		"same":  `^local$`,                     // same machine, found via the server
		"local": `^local$`,                     // same machine, no server at all
		"lan":   `^direct 192\.168\.1\.2:\d+$`, // LAN address behind the same NAT
		"nat":   `^direct 11\.0\.0\.20:\d+$`,   // hole punched through both NATs
		"relay": `^relay$`,                     // punching disabled
	}
	resRe := regexp.MustCompile(`RESULT path=(.+) connect=(\S+) rtt=(\S+)`)
	for name, pat := range want {
		b, _ := os.ReadFile(filepath.Join(out, name+".log"))
		m := resRe.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s: no result\n%s", name, strings.TrimSpace(string(b)))
			continue
		}
		if !regexp.MustCompile(pat).MatchString(m[1]) {
			t.Errorf("%s: path %q, want %s", name, m[1], pat)
			continue
		}
		t.Logf("%-5s path=%-24s connect=%-10s rtt=%s", name, m[1], m[2], m[3])
	}
}
