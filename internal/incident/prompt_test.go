package incident

import (
	"strings"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func TestBuildPromptRequiresChineseHumanReadableOutput(t *testing.T) {
	prompt := BuildPrompt(model.AlertEnvelope{}, nil, "", "abc123", false, "")
	for _, want := range []string{
		"所有面向人的内容必须使用简体中文",
		"除固定枚举和技术标识符外，所有字符串内容必须使用简体中文",
		`"summary": "简洁的中文结论"`,
		`"reason":"中文关联说明"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain %q", want)
		}
	}
}
