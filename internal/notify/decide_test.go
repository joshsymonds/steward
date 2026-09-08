package notify

import "testing"

func TestDecideCompletionRootsAndStructuralSuppressions(t *testing.T) {
	tests := []struct {
		name string
		in   HookInput
		scan ScanResult
		want Decision
	}{
		{
			name: "Claude root Stop sends done",
			in:   HookInput{Harness: harnessClaude, HookEventName: eventStop},
			want: Decision{Outcome: OutcomeSend, Urgency: UrgencyDone, Reason: "root completion"},
		},
		{
			name: "Pi root TurnComplete sends done",
			in:   HookInput{Harness: harnessPi, HookEventName: eventTurnComplete},
			want: Decision{Outcome: OutcomeSend, Urgency: UrgencyDone, Reason: "root completion"},
		},
		{
			name: "Pi root TurnComplete sends done",
			in:   HookInput{Harness: harnessPi, HookEventName: eventTurnComplete},
			want: Decision{Outcome: OutcomeSend, Urgency: UrgencyDone, Reason: "root completion"},
		},
		{
			name: "active Claude goal is structurally silent",
			in:   HookInput{Harness: harnessClaude, HookEventName: eventStop},
			scan: ScanResult{Goal: GoalState{Status: GoalActive}},
			want: Decision{Outcome: OutcomeSilent, Reason: "goal active: deferring to built-in /goal"},
		},
		{
			name: "AgentID child is silent",
			in: HookInput{
				Harness: harnessPi, HookEventName: eventTurnComplete, AgentID: "child-1",
			},
			want: Decision{Outcome: OutcomeSilent, Reason: "agent context"},
		},
		{
			name: "AgentType child is silent even without AgentID",
			in: HookInput{
				Harness: harnessClaude, HookEventName: eventStop, AgentType: "worker",
			},
			want: Decision{Outcome: OutcomeSilent, Reason: "agent context"},
		},
		{
			name: "SubagentStop is silent",
			in:   HookInput{Harness: harnessClaude, HookEventName: "SubagentStop"},
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled event: SubagentStop"},
		},
		{
			name: "tool loop event is silent",
			in:   HookInput{Harness: harnessClaude, HookEventName: "PostToolUse"},
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled event: PostToolUse"},
		},
		{
			name: "session end cleanup is silent",
			in:   HookInput{Harness: harnessClaude, HookEventName: eventSessionEnd},
			want: Decision{Outcome: OutcomeSilent, Reason: "session end"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.in, tt.scan); got != tt.want {
				t.Fatalf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecideExplicitInputAndUnrelatedNotifications(t *testing.T) {
	tests := []struct {
		name             string
		notificationType string
		message          string
		agentID          string
		agentType        string
		want             Decision
	}{
		{
			name: "permission prompt", notificationType: "permission_prompt", message: "allow command?",
			want: Decision{
				Outcome: OutcomeSend, Urgency: UrgencyBlocked,
				Message: "allow command?", Reason: "permission prompt",
			},
		},
		{
			name: "elicitation dialog", notificationType: "elicitation_dialog", message: "choose a region",
			want: Decision{
				Outcome: OutcomeSend, Urgency: UrgencyBlocked,
				Message: "choose a region", Reason: "elicitation dialog",
			},
		},
		{
			name: "elicitation URL dialog", notificationType: "elicitation_url_dialog", message: "open the form",
			want: Decision{
				Outcome: OutcomeSend, Urgency: UrgencyBlocked,
				Message: "open the form", Reason: "elicitation dialog",
			},
		},
		{
			name: "elicitation URL dialog AgentID child", notificationType: "elicitation_url_dialog",
			message: "open the form", agentID: "child-1",
			want: Decision{Outcome: OutcomeSilent, Reason: "agent context"},
		},
		{
			name: "elicitation URL dialog AgentType child", notificationType: "elicitation_url_dialog",
			message: "open the form", agentType: "worker",
			want: Decision{Outcome: OutcomeSilent, Reason: "agent context"},
		},
		{
			name: "agent needs input", notificationType: "agent_needs_input", message: "answer the worker",
			want: Decision{
				Outcome: OutcomeSend, Urgency: UrgencyBlocked,
				Message: "answer the worker", Reason: "background session needs input",
			},
		},
		{
			name: "auth success", notificationType: "auth_success",
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled notification type: auth_success"},
		},
		{
			name: "idle prompt", notificationType: "idle_prompt",
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled notification type: idle_prompt"},
		},
		{
			name: "broadcast completed", notificationType: "agent_completed",
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled notification type: agent_completed"},
		},
		{
			name: "unrelated", notificationType: "something_else",
			want: Decision{Outcome: OutcomeSilent, Reason: "unhandled notification type: something_else"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := HookInput{
				Harness: harnessClaude, HookEventName: eventNotification,
				NotificationType: tt.notificationType, Message: tt.message,
				AgentID: tt.agentID, AgentType: tt.agentType,
			}
			if got := Decide(in, ScanResult{}); got != tt.want {
				t.Fatalf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecideCompletionTextNeverChangesEligibilityOrUrgency(t *testing.T) {
	texts := []string{
		"",
		"I am still working and will continue using tools.",
		"The task is completely finished.",
		"Please approve access before I can continue.",
	}
	for _, text := range texts {
		in := HookInput{
			Harness: harnessPi, HookEventName: eventTurnComplete,
			LastAssistantMessage: text,
		}
		got := Decide(in, ScanResult{})
		if got.Outcome != OutcomeSend || got.Urgency != UrgencyDone {
			t.Errorf("text %q: Decide() = %+v, want deterministic send/done", text, got)
		}
	}
}
