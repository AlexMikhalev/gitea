// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package quarto

import (
	"bytes"
)

func HasFrontmatter(data []byte) bool {
	return bytes.HasPrefix(data, []byte("---"))
}

func ExtractFrontmatter(data []byte) (frontmatter []byte, content []byte) {
	if !HasFrontmatter(data) {
		return nil, data
	}

	rest := data[3:]
	endIdx := bytes.Index(rest, []byte("\n---"))
	if endIdx < 0 {
		return nil, data
	}

	frontmatter = rest[:endIdx]
	content = bytes.TrimSpace(rest[endIdx+4:])
	return frontmatter, content
}

func StripFrontmatter(data []byte) []byte {
	_, content := ExtractFrontmatter(data)
	return content
}
