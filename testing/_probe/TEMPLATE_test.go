// PROBE — temporary. Delete or promote at CLEANUP. Topic: ___  Date: ___  Question: ___
//
// Copy to <topic>_<date>_test.go and run:
//   go test -race -count=1 -v ./testing/_probe/ -run TestProbe
// Assert nothing you don't yet know. Log what you see, read it, then decide.
package probe

import "testing"

func TestProbeTemplate(t *testing.T) {
	t.Skip("template — copy me; integration.StartLeader(t) and reader.New arrive with S2/S3")
}
