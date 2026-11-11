package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoConsumerShapes (S9 criterion 5, D-044): no non-test source under internal/, reader/,
// protocol/ or cmd/ names a consumer's key segment, and the index package splits keys on no
// separator. Consumer-shaped data belongs only in testing/conformance.
func TestNoConsumerShapes(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "reader", "protocol", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(b)
			if strings.Contains(strings.ToLower(src), "patient") {
				t.Errorf("%s names the consumer segment \"patient\"", path)
			}
			if filepath.Base(filepath.Dir(path)) == "cache" && strings.Contains(src, "':'") {
				t.Errorf("%s splits keys on ':' (the index must be shape-free)", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
