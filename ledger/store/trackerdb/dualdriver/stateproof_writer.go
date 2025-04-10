// Copyright (C) 2019-2025 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package dualdriver

import (
	"context"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
)

type stateproofWriter struct {
	primary   trackerdb.SpVerificationCtxWriter
	secondary trackerdb.SpVerificationCtxWriter
}

// StoreSPContexts implements trackerdb.SpVerificationCtxWriter
func (w *stateproofWriter) StoreSPContexts(ctx context.Context, verificationContext []*ledgercore.StateProofVerificationContext) error {
	errP := w.primary.StoreSPContexts(ctx, verificationContext)
	errS := w.secondary.StoreSPContexts(ctx, verificationContext)
	// coalesce errors
	return coalesceErrors(errP, errS)
}

// StoreSPContextsToCatchpointTbl implements trackerdb.SpVerificationCtxWriter
func (w *stateproofWriter) StoreSPContextsToCatchpointTbl(ctx context.Context, verificationContexts []ledgercore.StateProofVerificationContext) error {
	errP := w.primary.StoreSPContextsToCatchpointTbl(ctx, verificationContexts)
	errS := w.secondary.StoreSPContextsToCatchpointTbl(ctx, verificationContexts)
	// coalesce errors
	return coalesceErrors(errP, errS)
}

// DeleteOldSPContexts implements trackerdb.SpVerificationCtxWriter
func (w *stateproofWriter) DeleteOldSPContexts(ctx context.Context, earliestLastAttestedRound basics.Round) error {
	errP := w.primary.DeleteOldSPContexts(ctx, earliestLastAttestedRound)
	errS := w.secondary.DeleteOldSPContexts(ctx, earliestLastAttestedRound)
	// coalesce errors
	return coalesceErrors(errP, errS)
}
