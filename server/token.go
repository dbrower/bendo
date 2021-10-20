package server

import (
	"bufio"
	"io"
	"os"
	"sort"
	"strings"
)

// A TokenValidator validates and decodes user tokens passed into the web API. If
// the given token is not valid, for whatever reason, the user "" with a role of
// RoleUnknown is returned. An error is returned only if there is some kind of error doing
// the lookup and the ultimate status of the token is unknown.
type TokenValidator interface {
	TokenValid(token string) (user string, role Role, err error)
}

// A Role is an enumeration describing the permission level a given user has.
type Role int

// The enumerations for a Role. They form a linear order with later entries
// having more permissions than earlier ones.
const (
	RoleUnknown Role = iota
	RoleMDOnly
	RoleRead
	RoleWrite
	RoleAdmin
)

// AtoRole converts a string into a Role. The strings are case-insensitive,
// and are "mdonly", "read", "write", "admin". If the string cannot be decoded
// RoleUnknown is returned.
func AtoRole(s string) Role {
	switch strings.ToLower(s) {
	case "mdonly":
		return RoleMDOnly
	case "read":
		return RoleRead
	case "write":
		return RoleWrite
	case "admin":
		return RoleAdmin
	default:
		return RoleUnknown
	}
}

// A NobodyValidator is a TokenValidator that for every possible token
// returns a user named "nobody" with the Admin role.
type NobodyValidator struct{}

// TokenValid will always return an Admin user named "nobody".
func (NobodyValidator) TokenValid(token string) (string, Role, error) {
	return "nobody", RoleAdmin, nil
}

// An InvalidValidator is a TokenValidator for which every token is
// invalid. That is, it always returns the user "" with the Unknown role.
type InvalidValidator struct{}

// TokenValid will always return an invalid user with no permissions.
func (InvalidValidator) TokenValid(token string) (string, Role, error) {
	return "", RoleUnknown, nil
}

// NewListValidator returns a validator backed by a predefined list of users,
// which are read from r upon creation. The reader r should consist of a
// sequence of user entries, separated by newlines. Each entry has the form:
//
//     <user name>  <role>  <token>
//
// The fields are delineated by whitespace (spaces or tabs). This decoder does
// not permit spaces in either the user name or the token. The role is one of
// "MDOnly", "Read", "Write", "Admin" (case insensitive). Empty lines and lines
// beginning with a hash '#' are skipped.
func NewListValidator(r io.Reader) (TokenValidator, error) {
	users, err := parseListFile(r)
	if err != nil {
		return nil, err
	}
	sort.Sort(byToken(users))
	return listValidator{users}, nil
}

// NewListValidatorFile is a convenience function that reads the contents of
// the given file into a ListValidator. The file should have the same format
// that NewListValidator expects.
func NewListValidatorFile(fname string) (TokenValidator, error) {
	f, err := os.Open(fname)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return NewListValidator(f)
}

// NewListValidatorString is a convenience function that passes the given string
// into a ListValidator. The format of the string is the same as that expected
// by NewListValidator.
func NewListValidatorString(data string) (TokenValidator, error) {
	return NewListValidator(strings.NewReader(data))
}

func parseListFile(r io.Reader) ([]userEntry, error) {
	var result []userEntry
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// split on whitespace
		pieces := strings.Fields(scanner.Text())
		// skip blank lines or lines beginning with a '#'
		if len(pieces) == 0 || pieces[0][0] == '#' {
			continue
		}
		if len(pieces) != 3 {
			// wrong number of columns
			continue
		}
		result = append(result, userEntry{
			token: pieces[2],
			user:  pieces[0],
			role:  AtoRole(pieces[1]),
		})
	}
	return result, scanner.Err()
}

type listValidator struct {
	data []userEntry
}

type byToken []userEntry

func (ue byToken) Len() int           { return len(ue) }
func (ue byToken) Less(i, j int) bool { return ue[i].token < ue[j].token }
func (ue byToken) Swap(i, j int)      { ue[i], ue[j] = ue[j], ue[i] }

type userEntry struct {
	token string
	user  string
	role  Role
}

func (ld listValidator) TokenValid(token string) (string, Role, error) {
	users := ld.data
	i := sort.Search(len(users), func(i int) bool { return users[i].token >= token })
	if i < len(users) && users[i].token == token {
		return users[i].user, users[i].role, nil
	}
	return "", RoleUnknown, nil
}

// A ValidatorMux allows many validators to be combined. It returns the highest
// role (and associated username) it finds for a given token. If any error is
// encountered, the error is returned instead.
type ValidatorMux struct {
	Validators []TokenValidator
}

// Add the provided TokenValidator to the end of the search list. There is no
// protecting mutex, so do not call if other goroutines could be calling
// TokenValid()
func (mux *ValidatorMux) Add(v TokenValidator) {
	mux.Validators = append(mux.Validators, v)
}

func (mux *ValidatorMux) TokenValid(token string) (string, Role, error) {
	user := ""
	role := RoleUnknown
	for _, v := range mux.Validators {
		u, r, err := v.TokenValid(token)
		if err != nil {
			return "", RoleUnknown, err
		}
		if r > role {
			user = u
			role = r
		}
	}
	return user, role, nil
}
