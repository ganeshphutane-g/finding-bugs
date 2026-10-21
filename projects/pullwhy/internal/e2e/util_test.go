package e2e

import "encoding/base64"

func basicB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
