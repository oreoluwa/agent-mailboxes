//go:build !unix

package main

import "errors"

func readPrivateAgentProfile(_ string, _ int64) ([]byte, error) {
	return nil, errors.New("file-backed email profiles require Unix file security; use EMAIL_API and secret-injected EMAIL_TOKEN on this platform")
}
