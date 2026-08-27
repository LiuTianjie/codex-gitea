package incident

import "testing"

func TestParseAnalysisResultKeepsAIAssessment(t *testing.T) {
	result, err := parseAnalysisResult(`{
		"summary":"核心接口回归",
		"confidence":"high",
		"assessed_severity":"HIGH",
		"severity_reason":"影响生产学习主链路",
		"impact_scope":["PROD 学生用户","练习提交接口"]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssessedSeverity != "high" || result.SeverityReason == "" || len(result.ImpactScope) != 2 {
		t.Fatalf("assessment = %+v", result)
	}
}

func TestParseAnalysisResultFallsBackWhenAssessmentMissing(t *testing.T) {
	result, err := parseAnalysisResult(`{"summary":"证据不足","confidence":"low"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssessedSeverity != "low" || result.SeverityReason == "" || len(result.ImpactScope) != 1 || len(result.EvidenceGaps) != 1 {
		t.Fatalf("fallback assessment = %+v", result)
	}
}
