//go:build !unix

package config

import "fmt"

func readMailboxCredentialKeyFile(string) ([]byte, error) {
	return nil, fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE is unsupported on this platform; use deployment secret injection")
}
