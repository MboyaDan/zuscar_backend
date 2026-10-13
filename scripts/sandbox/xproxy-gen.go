//go:build ignore

// Sandbox-only: builds a GOPROXY file tree entry for golang.org/x/<name>@<version> from the
// github.com/golang/<name> mirror, using the same module zip algorithm the real proxy uses.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

func sh(dir string, args ...string) string {
	c := exec.Command(args[0], args[1:]...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v: %v\n%s", args, err, out)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}

// mirror maps a vanity module path to its GitHub repository (sandbox cannot resolve vanity domains).
func mirror(path string) string {
	switch {
	case strings.HasPrefix(path, "golang.org/x/"):
		return "golang/" + strings.Split(strings.TrimPrefix(path, "golang.org/x/"), "/")[0]
	case strings.HasPrefix(path, "gopkg.in/yaml."):
		return "go-yaml/yaml"
	case strings.HasPrefix(path, "gopkg.in/check."):
		return "go-check/check"
	case strings.HasPrefix(path, "go.uber.org/"):
		return "uber-go/" + strings.Split(strings.TrimPrefix(path, "go.uber.org/"), "/")[0]
	case path == "google.golang.org/protobuf":
		return "protocolbuffers/protobuf-go"
	case strings.HasPrefix(path, "go.mongodb.org/mongo-driver"):
		return "mongodb/mongo-go-driver"
	case path == "google.golang.org/grpc":
		return "grpc/grpc-go"
	}
	return ""
}

var pseudo = regexp.MustCompile(`-([0-9a-f]{12})$`)

func main() {
	path, version, proxy := os.Args[1], os.Args[2], os.Args[3] // golang.org/x/arch v0.1.0 /tmp/gomodproxy
	gh := mirror(path)
	if gh == "" {
		fmt.Fprintln(os.Stderr, "no GitHub mirror known for", path)
		os.Exit(1)
	}
	repo := filepath.Join("/tmp/xrepos", strings.ReplaceAll(gh, "/", "_"))
	if _, err := os.Stat(repo); err != nil {
		os.MkdirAll("/tmp/xrepos", 0o755)
		sh("", "git", "clone", "-q", "--bare", "https://github.com/"+gh, repo)
	}
	rev := version
	if m := pseudo.FindStringSubmatch(version); m != nil {
		rev = m[1]
	}
	sh(repo, "git", "rev-parse", "--verify", rev+"^{commit}")
	ts := sh(repo, "git", "show", "-s", "--format=%cI", rev+"^{commit}")
	t, _ := time.Parse(time.RFC3339, ts)
	tmp, _ := os.MkdirTemp("", "x")
	defer os.RemoveAll(tmp)
	sh("", "bash", "-c", fmt.Sprintf("git -C %s archive %s | tar -x -C %s", repo, rev, tmp))
	dest := filepath.Join(proxy, path, "@v")
	os.MkdirAll(dest, 0o755)
	mod, err := os.ReadFile(filepath.Join(tmp, "go.mod"))
	if err != nil {
		mod = []byte("module " + path + "\n")
	}
	os.WriteFile(filepath.Join(dest, version+".mod"), mod, 0o644)
	info, _ := json.Marshal(map[string]string{"Version": version, "Time": t.UTC().Format(time.RFC3339)})
	os.WriteFile(filepath.Join(dest, version+".info"), info, 0o644)
	zf, _ := os.Create(filepath.Join(dest, version+".zip"))
	defer zf.Close()
	if err := modzip.CreateFromDir(zf, module.Version{Path: path, Version: version}, tmp); err != nil {
		fmt.Fprintln(os.Stderr, "zip:", err)
		os.Exit(1)
	}
	fmt.Println("generated", path, version)
}
