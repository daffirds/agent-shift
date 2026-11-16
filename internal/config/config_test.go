package config

import (
	"encoding/json"
	"testing"
)

func TestAgentJSONReadsLegacySnapshots(t *testing.T) {
	// How session.json stored agents before Agent had JSON tags.
	legacy := `{"Name":"triage","Engine":"claude","Workdir":"/w","Autonomy":"full","SystemPrompt":"do it",
		"Trigger":{"Workspace":"T1","Conversations":[{"ID":"C1","Name":"tickets"}]},
		"ThreadFrom":[{"ID":"me"}],"ExtraArgs":["--x"]}`
	var a Agent
	if err := json.Unmarshal([]byte(legacy), &a); err != nil {
		t.Fatal(err)
	}
	if a.Name != "triage" || a.SystemPrompt != "do it" || a.Trigger.Conversations[0].ID != "C1" || len(a.ThreadFrom) != 1 || a.ExtraArgs[0] != "--x" {
		t.Fatalf("legacy: %+v", a)
	}

	b, _ := json.Marshal(a)
	var back Agent
	if err := json.Unmarshal(b, &back); err != nil || back.SystemPrompt != "do it" || back.Trigger.Workspace != "T1" {
		t.Fatalf("round trip: %s %+v", b, back)
	}
}
