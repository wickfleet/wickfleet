package login

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"wickfleet/internal/atomicfile"
)

// SetupToken writes a fresh token's hash to path and returns the token. The
// first admin needs it on the login page within SetupMaxAge.
func SetupToken(path string) (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	return tok, atomicfile.Write(path, []byte(tokenID(tok)+"\n"), 0o600)
}

const usage = `usage:
  wickfleet-hub setup-token               print a one-time token for the first admin (valid 1 h)
  wickfleet-hub user add <name> <role>    add or change a user, password on stdin (role: viewer, operator, admin)
  wickfleet-hub user del <name>           remove a user
  wickfleet-hub user list                 users and roles
`

// CLI runs the setup-token and user subcommands against the state dir.
func CLI(stateDir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	users := filepath.Join(stateDir, "users.json")
	fail := func(err error) int {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch {
	case len(args) == 1 && args[0] == "setup-token":
		existing, err := loadUsers(users)
		if err != nil {
			return fail(err)
		}
		if len(existing) > 0 {
			return fail(fmt.Errorf("users exist already: add more with wickfleet-hub user add"))
		}
		tok, err := SetupToken(filepath.Join(stateDir, "setup-token"))
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "setup token, valid for 1 h, once: %s\nopen /hub/login and create the first admin with it\n", tok)
		return 0
	case len(args) == 4 && args[0] == "user" && args[1] == "add":
		name, role := args[2], args[3]
		if err := checkUser(name, role); err != nil {
			return fail(err)
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return fail(err)
		}
		pw := strings.TrimRight(line, "\r\n")
		if err := checkPassword(pw); err != nil {
			return fail(err)
		}
		all, err := loadUsers(users)
		if err != nil {
			return fail(err)
		}
		hash, err := Hash(pw)
		if err != nil {
			return fail(err)
		}
		all[name] = User{Name: name, Role: role, Hash: hash}
		if err := saveUsers(users, all); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s is %s\n", name, role)
		return 0
	case len(args) == 3 && args[0] == "user" && args[1] == "del":
		all, err := loadUsers(users)
		if err != nil {
			return fail(err)
		}
		if _, ok := all[args[2]]; !ok {
			return fail(fmt.Errorf("no user %q", args[2]))
		}
		delete(all, args[2])
		if err := saveUsers(users, all); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s removed, their sessions end now\n", args[2])
		return 0
	case len(args) == 2 && args[0] == "user" && args[1] == "list":
		all, err := loadUsers(users)
		if err != nil {
			return fail(err)
		}
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(stdout, "%s\t%s\n", n, all[n].Role)
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}
