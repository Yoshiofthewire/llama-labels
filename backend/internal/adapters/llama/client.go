package llama

import (
	"strings"
)

func SelectLabelFromText(allowedLabels []string, output string) string {
	if len(allowedLabels) == 0 {
		return ""
	}
	lowerOut := strings.ToLower(output)
	for _, label := range allowedLabels {
		if strings.EqualFold(label, "Questionable") && strings.Contains(lowerOut, "questionable") {
			return label
		}
	}
	for _, label := range allowedLabels {
		if strings.Contains(lowerOut, strings.ToLower(label)) {
			return label
		}
	}
	if strings.Contains(lowerOut, "important") {
		for _, label := range allowedLabels {
			if strings.EqualFold(label, "Important") {
				return label
			}
		}
	}
	return ""
}
