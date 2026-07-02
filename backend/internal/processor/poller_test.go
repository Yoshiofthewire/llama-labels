package processor

import (
	"testing"

	"llama-lab/backend/internal/config"
)

func TestShouldSendNotification(t *testing.T) {
	tests := []struct {
		name          string
		settings      config.NotificationSettings
		selectedLabel string
		keywords      []string
		want          bool
	}{
		{
			name:     "none mode never sends",
			settings: config.NotificationSettings{Mode: "none", Keywords: []string{"Urgent"}},
			want:     false,
		},
		{
			name:     "all mode always sends",
			settings: config.NotificationSettings{Mode: "all"},
			want:     true,
		},
		{
			name:          "keywords mode matches selected label",
			settings:      config.NotificationSettings{Mode: "keywords", Keywords: []string{"Urgent"}},
			selectedLabel: "urgent",
			want:          true,
		},
		{
			name:     "keywords mode matches mapped keyword",
			settings: config.NotificationSettings{Mode: "keywords", Keywords: []string{"billing"}},
			keywords: []string{"Invoices", "Billing"},
			want:     true,
		},
		{
			name:          "keywords mode does not match when nothing selected",
			settings:      config.NotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "support",
			keywords:      []string{"helpdesk"},
			want:          false,
		},
		{
			name:          "keywords mode does not send when uncategorized",
			settings:      config.NotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "",
			keywords:      nil,
			want:          false,
		},
		{
			name:          "keywords mode sends from selected label before mailbox keyword readback",
			settings:      config.NotificationSettings{Mode: "keywords", Keywords: []string{"urgent"}},
			selectedLabel: "urgent",
			keywords:      nil,
			want:          true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSendNotification(tc.settings, tc.selectedLabel, tc.keywords); got != tc.want {
				t.Fatalf("shouldSendNotification() = %v, want %v", got, tc.want)
			}
		})
	}
}
