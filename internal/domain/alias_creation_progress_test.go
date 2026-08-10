package domain

import (
	"context"
	"testing"
)

func TestReportAliasCreationProgressNormalizesPercent(t *testing.T) {
	var updates []AliasCreationProgressUpdate
	ctx := WithAliasCreationProgressReporter(
		context.Background(),
		func(update AliasCreationProgressUpdate) {
			updates = append(updates, update)
		},
	)

	ReportAliasCreationProgress(ctx, AliasCreationPhasePreparing, -10, 0)
	ReportAliasCreationProgress(ctx, AliasCreationPhaseReconciling, 140, 2)
	ReportAliasCreationProgress(nil, AliasCreationPhaseFailed, 50, 0)

	want := []AliasCreationProgressUpdate{
		{Phase: AliasCreationPhasePreparing, Percent: 0},
		{Phase: AliasCreationPhaseReconciling, Percent: 100, Attempt: 2},
	}
	if len(updates) != len(want) {
		t.Fatalf("updates = %#v, want %#v", updates, want)
	}
	for index := range want {
		if updates[index] != want[index] {
			t.Fatalf("updates[%d] = %#v, want %#v", index, updates[index], want[index])
		}
	}
}
