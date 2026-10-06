package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/migrations"
)

// The fixture is byte-for-byte git show 661a48b:migrations/009_bus.sql, not a
// reconstruction of an old schema or a manually patched migration ledger.
func busUpgradeFiles(t *testing.T, legacy bool) (all, prior []migrationFile) {
	t.Helper()
	all, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range all {
		if file.name > "010_cass_recovery.sql" {
			continue
		}
		if legacy && file.name == busMigrationName {
			raw, err := os.ReadFile("testdata/009_bus_661a48b.sql")
			if err != nil {
				t.Fatal(err)
			}
			file.sql = string(raw)
			file.checksum = fmt.Sprintf("%x", sha256.Sum256(raw))
			if file.checksum != busLegacyChecksum {
				t.Fatalf("historical fixture checksum = %s", file.checksum)
			}
		}
		prior = append(prior, file)
	}
	if len(prior) != 10 {
		t.Fatalf("historical prefix has %d files, want 10", len(prior))
	}
	return all, prior
}

func TestBusUpgradeHistoricalChecksums(t *testing.T) {
	all, prior := busUpgradeFiles(t, false)
	want := []string{
		"ec3bd868fa43304c7849bd386649de2dfb39c17c4df2048804db69f050a09fa2",
		"142c79c8fba78aa83f61fe62c302a6970a8f9775ed48894c15de23dd39be7387",
		"7b641b4b5e731c95bcefe548e39c1c0d4c4021596c9763786c78457131f5baca",
		"158dd226196b600ec5a3ebec7b8364922914fb76b04d40f1effa6392968ae5b4",
		"77be16815b5248d0723a782d5a860bc10c534add75aaac55735fc36f5753c583",
		"d4d853a602156e342fb2fc75e091a0b3a07f1b11fbb606984007646a1feeb24b",
		"8d9f6a132680a38a1db1a412cbcb614cb131291603272302e656edf6d1fbf59b",
		"9f693d951df3abd8a5e850798cfacc9f203d2b70220cac6577bc05d1ca674bbc",
		busCanonicalChecksum,
		"af4cfbcc10a9d98cc17fc4420c70fe8356b3363d15133d83eabe5e385a7adcdd",
	}
	for i, file := range prior {
		if file.checksum != want[i] {
			t.Errorf("%s checksum = %s, want %s", file.name, file.checksum, want[i])
		}
	}
	found := false
	for _, file := range all {
		if file.name == busUpgradeName {
			found = true
			if file.checksum != busUpgradeChecksum {
				t.Errorf("013 changed without updating its explicit recognition contract: %s", file.checksum)
			}
		}
	}
	if !found {
		t.Fatal("required 013 missing")
	}
	busUpgradeFiles(t, true) // Verify authentic fixture even without Postgres.
}

func TestBusUpgradeValidateAppliedPrefix(t *testing.T) {
	all, old := busUpgradeFiles(t, true)
	_, current := busUpgradeFiles(t, false)
	ledger := func(files []migrationFile) map[string]string {
		applied := make(map[string]string, len(files))
		for _, file := range files {
			applied[file.name] = file.checksum
		}
		return applied
	}
	tests := []struct {
		name string
		edit func(*[]migrationFile, map[string]string)
		want string
	}{
		{name: "authentic production prefix"},
		{name: "current main prefix", edit: func(_ *[]migrationFile, a map[string]string) { a[busMigrationName] = busCanonicalChecksum }},
		{name: "already upgraded production", edit: func(_ *[]migrationFile, a map[string]string) {
			for _, f := range all {
				if f.name > "010_cass_recovery.sql" {
					a[f.name] = f.checksum
				}
			}
		}},
		{name: "missing additive upgrade", edit: func(f *[]migrationFile, _ map[string]string) { *f = current }, want: "checksum mismatch"},
		{name: "edited additive upgrade", edit: func(f *[]migrationFile, _ map[string]string) {
			for i := range *f {
				if (*f)[i].name == busUpgradeName {
					(*f)[i].checksum = "edited"
				}
			}
		}, want: "checksum mismatch"},
		{name: "edited canonical 009", edit: func(f *[]migrationFile, _ map[string]string) { (*f)[8].checksum = "edited" }, want: "checksum mismatch"},
		{name: "unknown ledger 009", edit: func(_ *[]migrationFile, a map[string]string) { a[busMigrationName] = "unknown" }, want: "checksum mismatch"},
		{name: "edited other migration", edit: func(_ *[]migrationFile, a map[string]string) { a["008_reparse.sql"] = busLegacyChecksum }, want: "checksum mismatch"},
		{name: "missing prefix entry", edit: func(_ *[]migrationFile, a map[string]string) { delete(a, "008_reparse.sql") }, want: "contiguous prefix"},
		{name: "unknown ledger entry", edit: func(_ *[]migrationFile, a map[string]string) {
			delete(a, "010_cass_recovery.sql")
			a["999_unknown.sql"] = "x"
		}, want: "contiguous prefix"},
		{name: "older binary newer schema", edit: func(f *[]migrationFile, a map[string]string) { *f = old; a[busUpgradeName] = busUpgradeChecksum }, want: "older binary"},
		{name: "edited applied 013", edit: func(_ *[]migrationFile, a map[string]string) {
			for _, f := range all {
				if f.name > "010_cass_recovery.sql" {
					a[f.name] = f.checksum
				}
			}
			a[busUpgradeName] = "edited"
		}, want: "checksum mismatch"},
		{name: "integrated lower migrations pending", edit: func(f *[]migrationFile, _ map[string]string) {
			*f = append(*f, migrationFile{name: "011_prefix_test.sql", checksum: "prefix test"})
			sort.Slice(*f, func(i, j int) bool { return (*f)[i].name < (*f)[j].name })
		}},
		{name: "013 applied with missing lower migration", edit: func(f *[]migrationFile, a map[string]string) {
			*f = append(*f, migrationFile{name: "011_prefix_test.sql", checksum: "prefix test"})
			sort.Slice(*f, func(i, j int) bool { return (*f)[i].name < (*f)[j].name })
			a[busUpgradeName] = busUpgradeChecksum
		}, want: "contiguous prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := append([]migrationFile(nil), all...)
			applied := ledger(old)
			if tt.edit != nil {
				tt.edit(&files, applied)
			}
			err := validateAppliedPrefix(files, applied)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
		})
	}
}
