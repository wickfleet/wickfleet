package login

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/crypto/argon2"

	"wickfleet/internal/atomicfile"
	"wickfleet/internal/authz"
)

// User is one row of users.json. Hash is argon2id in the PHC string form.
type User struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Hash string `json:"hash"`
}

var (
	userRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)
	roles  = map[string]authz.Role{"viewer": authz.Viewer, "operator": authz.Operator, "admin": authz.Admin}
)

// Password bounds: long enough to resist guessing, short enough that hashing stays cheap.
const (
	MinPassword = 12
	MaxPassword = 256
)

func checkUser(name, role string) error {
	if !userRE.MatchString(name) {
		return fmt.Errorf("user %q: want a-z first, then a-z 0-9 . _ -, at most 32", name)
	}
	if _, ok := roles[role]; !ok {
		return fmt.Errorf("role %q: want viewer, operator or admin", role)
	}
	return nil
}

func checkPassword(pw string) error {
	if n := len([]rune(pw)); n < MinPassword || len(pw) > MaxPassword {
		return fmt.Errorf("password: %d to %d characters", MinPassword, MaxPassword)
	}
	return nil
}

// OWASP's argon2id floor: 19 MiB, 2 passes, 1 lane.
const (
	argonMem  = 19 * 1024
	argonTime = 2
	argonKey  = 32
)

// Hash returns the argon2id PHC string of pw with a fresh salt.
func Hash(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMem, 1, argonKey)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=1$%s$%s", argon2.Version, argonMem, argonTime, b64(salt), b64(key)), nil
}

// Verify reports whether pw matches the PHC string hash.
func Verify(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m > 1<<20 || t > 10 || p < 1 || p > 8 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummy is hashed against for an unknown user, so a wrong name costs as much as a wrong password.
var dummy, _ = Hash("not a password, only the cost")

// loadUsers reads users.json; a missing file is no users.
func loadUsers(path string) (map[string]User, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]User{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]User{}
	for _, u := range f.Users {
		if err := checkUser(u.Name, u.Role); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[u.Name] = u
	}
	return out, nil
}

func saveUsers(path string, users map[string]User) error {
	list := make([]User, 0, len(users))
	for _, u := range users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, err := json.MarshalIndent(map[string][]User{"users": list}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(raw, '\n'), 0o600)
}
