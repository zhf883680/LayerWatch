package vision

import "testing"

func TestParseResult(t *testing.T) {
	result, err := parseResult("```json\n{\"status\":\"spaghetti\",\"confidence\":0.91,\"reason\":\"丝料成团\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSpaghetti || result.Confidence != 0.91 || !result.Abnormal() {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestParseResultUnknownStatus(t *testing.T) {
	result, err := parseResult(`{"status":"not-a-status","confidence":2,"reason":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUnknown || result.Confidence != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}
