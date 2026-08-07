package diagnostics

import (
	"fmt"
	"strings"
	"testing"
)

type fakeHashes struct {
	data map[string]map[string]string
	err  error
}

func (f fakeHashes) HGetAll(key string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.data[key], nil
}

func TestModemSectionRendersIdentity(t *testing.T) {
	c := New(fakeHashes{data: map[string]map[string]string{
		"internet": {
			"sim-imei":  "359800030914123",
			"sim-iccid": "8949000000000000001",
			"sim-imsi":  "262010123456789",
		},
		"modem": {
			"operator-name": "Vodafone.de",
			"operator-code": "26202",
		},
	}})

	got := c.modemSection()

	if !strings.HasPrefix(got, "=== modem ===\n") {
		t.Errorf("missing section header in:\n%s", got)
	}

	rows := parseRows(got)
	for label, want := range map[string]string{
		"IMEI":          "359800030914123",
		"ICCID":         "8949000000000000001",
		"IMSI":          "262010123456789",
		"operator":      "Vodafone.de",
		"operator code": "26202",
		"access tech":   "-",
	} {
		if rows[label] != want {
			t.Errorf("%s = %q, want %q", label, rows[label], want)
		}
	}
}

// parseRows turns the "label:   value" lines back into a map, ignoring the
// section header and blank lines.
func parseRows(section string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		label, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "===") {
			continue
		}
		rows[strings.TrimSpace(label)] = strings.TrimSpace(value)
	}
	return rows
}

func TestModemSectionAlignsValues(t *testing.T) {
	c := New(fakeHashes{data: map[string]map[string]string{}})

	var columns []int
	for _, line := range strings.Split(c.modemSection(), "\n") {
		if i := strings.Index(line, " -"); i >= 0 {
			columns = append(columns, i)
		}
	}

	if len(columns) != len(modemRows) {
		t.Fatalf("got %d value rows, want %d", len(columns), len(modemRows))
	}
	for i, col := range columns {
		if col != columns[0] {
			t.Errorf("row %d value starts at column %d, want %d", i, col, columns[0])
		}
	}
}

func TestModemSectionMarksMissingFields(t *testing.T) {
	c := New(fakeHashes{data: map[string]map[string]string{}})

	got := c.modemSection()

	if strings.Count(got, " -\n") != len(modemRows) {
		t.Errorf("expected every row to render as %q, got:\n%s", "-", got)
	}
}

func TestModemSectionWithoutRedis(t *testing.T) {
	got := New(nil).modemSection()
	if !strings.Contains(got, "no Redis connection") {
		t.Errorf("expected a no-connection note, got:\n%s", got)
	}
}

func TestModemSectionReportsReadError(t *testing.T) {
	c := New(fakeHashes{err: fmt.Errorf("boom")})
	got := c.modemSection()
	if !strings.Contains(got, "ERROR: reading internet hash: boom") {
		t.Errorf("expected the read error, got:\n%s", got)
	}
}
