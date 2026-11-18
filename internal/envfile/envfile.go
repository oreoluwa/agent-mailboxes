package envfile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Load reads environment variables from path and sets only keys that are not
// already present in the process environment.
func Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return parseAndSet(b, false)
}

func parseAndSet(data []byte, override bool) error {
	s := bufio.NewScanner(bytes.NewReader(data))
	for lineNum := 1; s.Scan(); lineNum++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		idx := strings.Index(line, "=")
		if idx <= 0 {
			return fmt.Errorf(".env parse error on line %d: expected KEY=VALUE", lineNum)
		}

		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			return fmt.Errorf(".env parse error on line %d: empty key", lineNum)
		}

		if strings.HasPrefix(val, "\"") {
			u, err := strconv.Unquote(val)
			if err != nil {
				return fmt.Errorf(".env parse error on line %d: %w", lineNum, err)
			}
			val = u
		} else if strings.HasPrefix(val, "'") {
			if len(val) < 2 || !strings.HasSuffix(val, "'") {
				return fmt.Errorf(".env parse error on line %d: unterminated single-quoted value", lineNum)
			}
			val = val[1 : len(val)-1]
		}

		if !override {
			if _, ok := os.LookupEnv(key); ok {
				continue
			}
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("set env %s: %w", key, err)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	return nil
}
