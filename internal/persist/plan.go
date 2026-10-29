package persist

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
)

// defaultProject owns every autocreated sheet (the all-zero uuid seeded by migration 000001).
var defaultProject = sheet.DefaultProject

// stmtKind is one SQL statement the writer runs; each maps to one generated query.
type stmtKind uint8

const (
	stmtUpsertSheet   stmtKind = iota + 1 // registry.UpsertSheet
	stmtUpsertEntry                       // entries.UpsertEntry
	stmtDeleteEntry                       // entries.DeleteEntry
	stmtDeletePrefix                      // entries.DeleteEntriesByPrefix
	stmtDeleteSuffix                      // entries.DeleteEntriesBySuffix
	stmtDeleteEntries                     // entries.DeleteEntries: one OpExpire, every key in one statement
)

// stmt is one planned statement. match is the key, prefix or suffix, by kind.
type stmt struct {
	kind         stmtKind
	project      uuid.UUID
	sheet        string
	match        string
	keys         []string // stmtDeleteEntries
	value        json.RawMessage
	expiresAt    time.Time // zero = no expiry (NULL)
	passwordHash []byte
	autocreated  bool
}

// sheetKey names one (project, sheet, key) row.
type sheetKey struct {
	project uuid.UUID
	sheet   string
	key     string
}

// plan collapses ops, in the order the sheets applied them, into the statements one flush runs.
//
// Rules (each keeps the final table state equal to running every op in order):
//   - a set, delete or expire of key k removes any earlier set/delete of k in the batch and is
//     appended at the end: the earlier statement's effect on k is overwritten by the later one, and
//     only statements that touch k (prefix/suffix deletes covering k, the sheet's create) order
//     against it, all of which stay before it;
//   - a prefix or suffix delete removes every earlier set/delete of a key it covers in the same
//     sheet, then is appended; a set of a covered key after it is appended after it and survives;
//   - an expire is ONE multi-key delete (DeleteEntries): it removes any earlier set/delete of its
//     keys and is appended; a later op on one of its keys is appended after it and never removes it
//     (the statement still covers the other keys; running it first is harmless, order is kept);
//   - a sheet create is never collapsed or moved: entry rows reference the sheet row (FK), and every
//     entry op for the sheet arrives after its create.
//
// Ops are planned in arrival order across sheets, so the result keeps cross-sheet order too.
func plan(ops []sheet.Op) []stmt {
	out := make([]stmt, 0, len(ops))
	dead := make([]bool, 0, len(ops))
	live := make(map[sheetKey]int, len(ops)) // keyed statement for a row → its index in out

	keyed := func(kind stmtKind, op sheet.Op, key string) {
		k := sheetKey{op.ProjectID, op.Sheet, key}
		if i, ok := live[k]; ok {
			dead[i] = true
		}
		live[k] = len(out)
		s := stmt{kind: kind, project: op.ProjectID, sheet: op.Sheet, match: key}
		if kind == stmtUpsertEntry {
			s.value, s.expiresAt = op.Value, op.ExpiresAt
		}
		out = append(out, s)
		dead = append(dead, false)
	}
	covering := func(kind stmtKind, op sheet.Op, match string, covers func(key, match string) bool) {
		for k, i := range live {
			if k.project == op.ProjectID && k.sheet == op.Sheet && covers(k.key, match) {
				dead[i] = true
				delete(live, k)
			}
		}
		out = append(out, stmt{kind: kind, project: op.ProjectID, sheet: op.Sheet, match: match})
		dead = append(dead, false)
	}

	for _, op := range ops {
		switch op.Kind {
		case sheet.OpCreate:
			out = append(out, stmt{
				kind: stmtUpsertSheet, project: op.ProjectID, sheet: op.Sheet,
				passwordHash: op.PasswordHash, autocreated: op.ProjectID == defaultProject,
			})
			dead = append(dead, false)
		case sheet.OpSet:
			keyed(stmtUpsertEntry, op, op.Key)
		case sheet.OpDelete:
			keyed(stmtDeleteEntry, op, op.Key)
		case sheet.OpExpire:
			// The multi-key statement is never entered in live: nothing later removes it.
			for _, key := range op.Keys {
				k := sheetKey{op.ProjectID, op.Sheet, key}
				if i, ok := live[k]; ok {
					dead[i] = true
					delete(live, k)
				}
			}
			if len(op.Keys) > 0 {
				out = append(out, stmt{kind: stmtDeleteEntries, project: op.ProjectID, sheet: op.Sheet, keys: op.Keys})
				dead = append(dead, false)
			}
		case sheet.OpDeletePrefix:
			covering(stmtDeletePrefix, op, op.Prefix, strings.HasPrefix)
		case sheet.OpDeleteSuffix:
			covering(stmtDeleteSuffix, op, op.Suffix, strings.HasSuffix)
		}
	}

	kept := out[:0]
	for i, s := range out {
		if !dead[i] {
			kept = append(kept, s)
		}
	}
	return kept
}
