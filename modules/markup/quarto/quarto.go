// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package quarto

import (
	"bytes"
	"io"

	"code.gitea.io/gitea/modules/markup"
	"code.gitea.io/gitea/modules/markup/markdown"
	"code.gitea.io/gitea/modules/setting"
)

func init() {
	if !setting.Quarto.Enabled {
		return
	}
	markup.RegisterRenderer(Renderer{})
}

type Renderer struct{}

var _ markup.Renderer = (*Renderer)(nil)

func (Renderer) Name() string {
	return "quarto"
}

func (Renderer) NeedPostProcess() bool {
	return true
}

func (Renderer) FileNamePatterns() []string {
	return []string{"*.qmd"}
}

func (Renderer) SanitizerRules() []setting.MarkupSanitizerRule {
	return []setting.MarkupSanitizerRule{}
}

func (Renderer) Render(ctx *markup.RenderContext, input io.Reader, output io.Writer) error {
	buf, err := io.ReadAll(input)
	if err != nil {
		return err
	}

	buf = StripFrontmatter(buf)
	buf = StripQuartoOptions(buf)

	return markdown.Render(ctx, bytes.NewReader(buf), output)
}
