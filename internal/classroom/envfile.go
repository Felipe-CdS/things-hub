package classroom

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnvFile reads KEY=value lines from a sh-style env file (the same file
// thub-send sources): blank lines and # comments are skipped, an optional
// "export " prefix is dropped, and one level of single or double quotes is
// removed. A missing file yields an empty map. A file readable by group or
// others is refused.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o044 != 0 {
		return nil, fmt.Errorf("%s is readable by group or others; run: chmod 600 '%s'", path, path)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := parseEnvLine(sc.Text())
		if ok {
			out[k] = v
		}
	}
	return out, sc.Err()
}

func parseEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	k, v, ok := strings.Cut(line, "=")
	if !ok || k == "" || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return k, v, true
}

// SetEnvFileKey writes KEY=value into the env file, replacing an existing
// line for the key or appending one. The file is created if needed and
// always left with mode 0600. value must be safe unquoted in sh.
func SetEnvFileKey(path, key, value string) error {
	if strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()") {
		return fmt.Errorf("refusing to write %s: the value has characters sh would interpret", key)
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if len(data) > 0 {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	line := key + "=" + value
	replaced := false
	for i, l := range lines {
		if k, _, ok := parseEnvLine(l); ok && k == key {
			if !replaced {
				lines[i] = line
			} else {
				lines[i] = ""
			}
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
