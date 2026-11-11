package redlitev1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var protos = []string{"cache.proto", "replica.proto"}

func protoDir() string { return filepath.Join("..", "..", "proto", "redlite", "v1") }

// TestGeneratedCodeCurrent (D-048): the generated code must match the .proto files.
//
//   - Always: the .proto files hash to PROTO_SHA256, which `make proto` writes when it regenerates;
//     a .proto edited without regenerating fails here.
//   - When protoc and both Go plugins are on PATH: regenerate into a temp dir and compare byte for
//     byte with the checked-in files (a hand edit of a .pb.go fails here too).
func TestGeneratedCodeCurrent(t *testing.T) {
	var all []byte
	for _, p := range protos {
		b, err := os.ReadFile(filepath.Join(protoDir(), p))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	sum := sha256.Sum256(all)
	want, err := os.ReadFile("PROTO_SHA256")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("proto/ changed since protocol/redlitev1 was generated: run `make proto` (hash %s, recorded %s)", got, strings.TrimSpace(string(want)))
	}

	for _, tool := range []string{"protoc", "protoc-gen-go", "protoc-gen-go-grpc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Logf("%s not on PATH: byte comparison skipped (the hash check above ran)", tool)
			return
		}
	}
	out := t.TempDir()
	args := []string{"-I", filepath.Join("..", "..", "proto"), "--go_out=" + out, "--go-grpc_out=" + out,
		"--go_opt=module=github.com/CrimsonCoder42/red_lite_cache", "--go-grpc_opt=module=github.com/CrimsonCoder42/red_lite_cache"}
	for _, p := range protos {
		args = append(args, filepath.Join(protoDir(), p))
	}
	if b, err := exec.Command("protoc", args...).CombinedOutput(); err != nil {
		t.Fatalf("protoc: %v\n%s", err, b)
	}
	gen, _ := filepath.Glob(filepath.Join(out, "protocol", "redlitev1", "*.go"))
	if len(gen) == 0 {
		t.Fatal("protoc produced nothing")
	}
	for _, g := range gen {
		fresh, _ := os.ReadFile(g)
		checked, err := os.ReadFile(filepath.Base(g))
		if err != nil || !bytes.Equal(fresh, checked) {
			t.Errorf("%s differs from a fresh protoc run: run `make proto`", filepath.Base(g))
		}
	}
}
