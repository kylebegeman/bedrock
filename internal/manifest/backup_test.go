package manifest

import (
	"strings"
	"testing"
)

const withDataApp = `app: dragon-writer
workloads:
  web:
    kind: web
    image: local/dw:1
    port: 3000
    routes:
      - host: dragonwriter.begam.in
data:
  postgres:
    version: "16"
  volumes:
    uploads: {}
`

func TestBackupsDefaultForAnAppWithData(t *testing.T) {
	m, err := Parse([]byte(withDataApp))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasData() || !m.BackedUp() {
		t.Fatal("an app with data is backed up by default")
	}
	if m.BackupSchedule() != DefaultBackupSchedule || m.DrillSchedule() != DefaultDrillSchedule || m.KeepPolicy() != DefaultKeep {
		t.Fatalf("defaults: %s %s %+v", m.BackupSchedule(), m.DrillSchedule(), m.KeepPolicy())
	}
	if sql, _ := m.VerifyQuery(); sql != "" {
		t.Fatal("no verify query by default")
	}
	m2, err := Parse([]byte(withDataApp + `backup:
  schedule: "*/2 * * * *"
  drill: "0 5 * * 1"
  keep: {daily: 3, weekly: 1}
  verify:
    sql: select count(*) from story_chapters
    at_least: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	if m2.BackupSchedule() != "*/2 * * * *" || m2.DrillSchedule() != "0 5 * * 1" || m2.KeepPolicy().Monthly != 0 || m2.KeepPolicy().Daily != 3 {
		t.Fatalf("%+v", m2.Backup)
	}
	if sql, atLeast := m2.VerifyQuery(); !strings.HasPrefix(sql, "select count") || atLeast != 30 {
		t.Fatalf("%q %d", sql, atLeast)
	}
	off, err := Parse([]byte(withDataApp + "backup:\n  off: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if off.BackedUp() {
		t.Fatal("off means off")
	}
}

func TestBackupSectionIsValidated(t *testing.T) {
	cases := []struct{ yaml, want string }{
		{withDataApp + "backup:\n  schedule: often\n", "backup.schedule"},
		{withDataApp + "backup:\n  drill: \"99 * * * *\"\n", "backup.drill"},
		{withDataApp + "backup:\n  keep: {daily: 0}\n", "at least one"},
		{withDataApp + "backup:\n  verify:\n    sql: \"  \"\n", "sql is required"},
		{strings.Replace(withDataApp, "  postgres:\n    version: \"16\"\n", "", 1) + "backup:\n  verify:\n    sql: select 1\n", "needs a database"},
		{`app: site
workloads:
  pages:
    kind: static
    dir: public
    routes:
      - host: site.example
backup:
  schedule: "0 3 * * *"
`, "keeps no data"},
		{strings.Replace(withDataApp, "app: dragon-writer", "app: bedrock", 1), "a name bedrock uses for itself"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
}
