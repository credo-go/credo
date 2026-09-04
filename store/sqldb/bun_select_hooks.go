package sqldb

import (
	"context"

	"github.com/uptrace/bun"
)

// This file, with bun_select_clone.go, is the Bun compatibility boundary of
// the package: code that depends on Bun's private layout or on its internal
// execution order rather than on its public API. Credo's own query policy
// (logical count, transaction selection, error mapping, pagination) lives in
// the query_*.go files and reaches Bun internals only through the helpers
// defined here. Both files are structurally pinned to Bun v1.2.18; follow the
// upgrade protocol in bun_select_clone.go before bumping the requirement.

// runBunSelectHooksBefore mirrors the pre-execution half of Bun's SELECT hook
// lifecycle on a private query snapshot and returns the AfterSelect hook the
// caller must invoke after execution (nil when the model has none). The table
// model is re-read after each step because BeforeSelect may replace the model
// and Bun resolves BeforeAppendModel and AfterSelect from the post-hook state.
//
// Bun pin: this order is Bun v1.2.18's (SelectQuery.Scan → beforeSelectHook →
// BeforeAppendModel → afterSelectHook). It is not a public contract; re-verify
// it against the pinned release when bumping Bun, following the upgrade
// protocol documented in bun_select_clone.go.
func runBunSelectHooksBefore(ctx context.Context, source *bun.SelectQuery) (bun.AfterSelectHook, error) {
	tableModel, err := bunSelectQueryTableModel(source)
	if err != nil {
		return nil, err
	}
	if tableModel != nil {
		if beforeSelect, ok := tableModel.Table().ZeroIface.(bun.BeforeSelectHook); ok {
			if hookErr := beforeSelect.BeforeSelect(ctx, source); hookErr != nil {
				return nil, hookErr
			}
		}
	}
	tableModel, err = bunSelectQueryTableModel(source)
	if err != nil {
		return nil, err
	}
	if tableModel != nil {
		if appendErr := tableModel.BeforeAppendModel(ctx, source); appendErr != nil {
			return nil, appendErr
		}
	}
	tableModel, err = bunSelectQueryTableModel(source)
	if err != nil {
		return nil, err
	}
	if tableModel == nil {
		return nil, nil
	}
	afterSelect, _ := tableModel.Table().ZeroIface.(bun.AfterSelectHook)
	return afterSelect, nil
}
