package engine

import "testing"

func TestDialectForDSN(t *testing.T) {
	cases := []struct {
		dsn  string
		want string
		ok   bool
	}{
		{"postgres://u:p@localhost:5434/shop?sslmode=disable", "postgres", true},
		{"postgresql://u:p@localhost/shop", "postgres", true},
		{"host=localhost dbname=shop user=u", "postgres", true},
		{"mysql://u:p@tcp(localhost:3306)/shop", "mysql", true},
		{"u:p@tcp(localhost:3306)/shop?parseTime=true", "mysql", true},
		{"u:p@unix(/tmp/mysql.sock)/shop", "mysql", true},
		{"jdbc:oracle:thin:@host:1521:orcl", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := DialectForDSN(tc.dsn)
		if tc.ok && err != nil {
			t.Errorf("DialectForDSN(%q) errored: %v", tc.dsn, err)
			continue
		}
		if !tc.ok && err == nil {
			t.Errorf("DialectForDSN(%q) = %q, want an error", tc.dsn, got)
			continue
		}
		if got != tc.want {
			t.Errorf("DialectForDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
}

func TestNormaliseMySQLDSN(t *testing.T) {
	const want = "u:p@tcp(localhost:3306)/shop"
	if got := normaliseMySQLDSN("mysql://" + want); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := normaliseMySQLDSN(want); got != want {
		t.Errorf("unprefixed DSN was altered: %q", got)
	}
}
