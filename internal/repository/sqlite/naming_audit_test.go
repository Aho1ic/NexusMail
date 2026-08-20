//go:build sqlite_fts5

package sqlite

import (
	"sync"
	"testing"

	"nexusmail/internal/domain"

	"gorm.io/gorm/schema"
)

// TestModelColumnsMatchSchema guards against a field whose GORM-derived column
// name does not exist in the table. Nothing fails loudly when that happens: the
// value is simply never read back, and the code that depends on it silently takes
// the "unset" branch forever.
func TestModelColumnsMatchSchema(t *testing.T) {
	store := openTestStore(t)
	models := []any{&domain.Account{}, &domain.Mailbox{}, &domain.Message{}, &domain.Attachment{},
		&domain.Draft{}, &domain.DraftAttachment{}, &domain.BlobObject{}}
	for _, model := range models {
		parsed, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		if err := store.db.Raw("SELECT name FROM pragma_table_info(?)", parsed.Table).Scan(&columns).Error; err != nil {
			t.Fatal(err)
		}
		if len(columns) == 0 {
			t.Errorf("table %s has no columns; is the table name right?", parsed.Table)
			continue
		}
		known := make(map[string]struct{}, len(columns))
		for _, column := range columns {
			known[column] = struct{}{}
		}
		for _, field := range parsed.Fields {
			if field.DBName == "" {
				continue
			}
			if _, ok := known[field.DBName]; !ok {
				t.Errorf("%s.%s maps to column %q which %s does not have", parsed.Table, field.Name, field.DBName, parsed.Table)
			}
		}
	}
}
