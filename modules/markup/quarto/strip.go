// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package quarto

import (
	"regexp"
)

var quartoOptionRE = regexp.MustCompile(`(?m)^#\|.*$\n?`)

func StripQuartoOptions(data []byte) []byte {
	return quartoOptionRE.ReplaceAll(data, nil)
}
