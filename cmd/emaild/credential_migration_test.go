package main

import "testing"

func TestParseCredentialMigrationArgs(t *testing.T) {
	ids, err := parseCredentialMigrationArgs([]string{"--plaintext-mailbox-id", "first", "--plaintext-mailbox-id", "second"})
	if err != nil || len(ids) != 2 || ids[0] != "first" || ids[1] != "second" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	for _, args := range [][]string{{"--plaintext-mailbox-id", " "}, {"unexpected"}, {"--unknown"}} {
		if _, err := parseCredentialMigrationArgs(args); err == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
	ids, err = parseCredentialMigrationArgs(nil)
	if err != nil || len(ids) != 0 {
		t.Fatalf("default must not override plaintext IDs: %v %v", ids, err)
	}
}
