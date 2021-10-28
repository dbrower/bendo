package server

import (
	"strings"
	"testing"
)

func TestAtoRole(t *testing.T) {
	var table = []struct {
		input  string
		output Role
	}{
		{"MDOnly", RoleMDOnly},
		{"mdonly", RoleMDOnly},
		{"read", RoleRead},
		{"Read", RoleRead},
		{"Write", RoleWrite},
		{"write", RoleWrite},
		{"admin", RoleAdmin},
		{"Admin", RoleAdmin},
		{"other", RoleUnknown},
	}

	for _, row := range table {
		result := AtoRole(row.input)
		if result != row.output {
			t.Errorf("For %v received %v, expected %v", row.input, result, row.output)
		}
	}
}

func TestTokenMux(t *testing.T) {
	var lists = []string{
		`a mdonly 123
		 b write 234
		 c admin 345
		 d read 456`,
		`bb read 234
		 c admin 345
		 dd admin 456
		 e write 567`,
		`b read 234
		 cc admin 345
		 f mdonly 678`,
	}
	mux := ValidatorMux{}
	for _, list := range lists {
		d, err := NewListValidatorString(list)
		if err != nil {
			t.Fatalf("Received %s", err)
		}
		mux.Add(d)
	}

	var table = []struct {
		input string
		user  string
		role  Role
	}{
		{"123", "a", RoleMDOnly}, // in first list
		{"567", "e", RoleWrite},  // in second list
		{"---", "", RoleUnknown}, // in no lists
		{"234", "b", RoleWrite},  // return highest role
		{"345", "c", RoleAdmin},  // use first name for highest role
		{"456", "dd", RoleAdmin}, // use highest role name
		{"678", "f", RoleMDOnly}, // query every list
	}

	for _, row := range table {
		t.Logf("Testing %v", row)
		user, role, err := mux.TokenValid(row.input)
		if err != nil {
			t.Errorf("Received error %s", err.Error())
		}
		if user != row.user || role != row.role {
			t.Errorf("Received %s, %v, expected %v", user, role, row)
		}
	}
}

func TestListValid(t *testing.T) {
	d, err := NewListValidatorString(`a  mdonly  123
	b write 234
	c admin 345
	d read 456`)
	if err != nil {
		t.Fatalf("Received %s", err.Error())
	}

	var table = []struct {
		input string
		user  string
		role  Role
	}{
		{"123", "a", RoleMDOnly},
		{" 123", "", RoleUnknown},
		{"123 ", "", RoleUnknown},
		{"456", "d", RoleRead},
		{"234", "b", RoleWrite},
		{"345", "c", RoleAdmin},
		{"3456", "", RoleUnknown},
	}

	for _, row := range table {
		t.Logf("Testing %v", row)
		user, role, err := d.TokenValid(row.input)
		if err != nil {
			t.Errorf("Received error %s", err.Error())
		}
		if user != row.user || role != row.role {
			t.Errorf("Received %s, %v, expected %v", user, role, row)
		}
	}
}

func TestParseListFile(t *testing.T) {
	var table = []struct {
		input  string
		output []userEntry
	}{
		{`adam  write  0123456789`,
			[]userEntry{
				{token: "0123456789",
					user: "adam",
					role: RoleWrite,
				},
			},
		},
		{`  # test file
			qwerty   Admin  1234567

			zxcv  mdonly  23456
			#asdfg write  34567
`,
			[]userEntry{{
				token: "1234567",
				user:  "qwerty",
				role:  RoleAdmin,
			}, {
				token: "23456",
				user:  "zxcv",
				role:  RoleMDOnly,
			},
			},
		},
		{`person bad-role 1234567890  `,
			[]userEntry{{
				token: "1234567890",
				user:  "person",
				role:  RoleUnknown,
			},
			},
		},
		{`field1    field2   field3 field4`, []userEntry{}},
		{`     field1    field2   `, []userEntry{}},
	}

	for i, row := range table {
		result, err := parseListFile(strings.NewReader(row.input))
		if err != nil {
			t.Errorf("For row %d received error %s", i, err.Error())
		}
		if !userEntryEqual(result, row.output) {
			t.Errorf("For row %d received output %v", i, result)
		}
	}
}

func userEntryEqual(a, b []userEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
