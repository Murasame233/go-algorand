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

package catchup

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/committee"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/network"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/rpcs"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/execpool"
)

var defaultConfig = config.GetDefaultLocal()
var poolAddr = basics.Address{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
var sinkAddr = basics.Address{0x7, 0xda, 0xcb, 0x4b, 0x6d, 0x9e, 0xd1, 0x41, 0xb1, 0x75, 0x76, 0xbd, 0x45, 0x9a, 0xe6, 0x42, 0x1d, 0x48, 0x6d, 0xa3, 0xd4, 0xef, 0x22, 0x47, 0xc4, 0x9, 0xa3, 0x96, 0xb8, 0x2e, 0xa2, 0x21}

// Mocked Fetcher will mock UniversalFetcher
type MockedFetcher struct {
	ledger      Ledger
	timeout     bool
	tries       map[basics.Round]int
	latency     time.Duration
	predictable bool
	mu          deadlock.Mutex
}

func (m *MockedFetcher) fetchBlock(ctx context.Context, round basics.Round, peer network.Peer) (*bookkeeping.Block, *agreement.Certificate, time.Duration, error) {
	if m.OutOfPeers(round) {
		return nil, nil, time.Duration(0), nil
	}
	if m.timeout {
		time.Sleep(time.Duration(config.GetDefaultLocal().CatchupHTTPBlockFetchTimeoutSec)*time.Second + time.Second)
	}
	time.Sleep(m.latency)

	if !m.predictable {
		// Add random delay to get it out of sync
		time.Sleep(time.Duration(rand.Int()%50) * time.Millisecond)
	}
	block, err := m.ledger.Block(round)
	if round > m.ledger.LastRound() {
		return nil, nil, time.Duration(0), errors.New("no block")
	} else if err != nil {
		panic(err)
	}

	var cert agreement.Certificate
	cert.Proposal.BlockDigest = block.Digest()
	return &block, &cert, time.Duration(0), nil
}

func (m *MockedFetcher) NumPeers() int {
	return 10
}

func (m *MockedFetcher) OutOfPeers(round basics.Round) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.tries[round]; !ok {
		m.tries[round] = 0
	}
	m.tries[round]++
	return m.tries[round] > m.NumPeers()
}

func (m *MockedFetcher) Close() { // noop
}

type mockedAuthenticator struct {
	mu deadlock.Mutex

	errorRound int
	fail       bool
}

func (auth *mockedAuthenticator) Authenticate(blk *bookkeeping.Block, cert *agreement.Certificate) error {
	auth.mu.Lock()
	defer auth.mu.Unlock()

	if (auth.errorRound >= 0 && basics.Round(auth.errorRound) == blk.Round()) || auth.fail {
		// change reply so that block is malformed
		return errors.New("mockedAuthenticator: block is malformed")
	}
	return nil
}

func (auth *mockedAuthenticator) Quit() {}

func (auth *mockedAuthenticator) alter(errorRound int, fail bool) {
	auth.mu.Lock()
	defer auth.mu.Unlock()

	auth.errorRound = errorRound
	auth.fail = fail
}

func TestServiceFetchBlocksSameRange(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledgers
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, 10)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	syncer := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: -1}, nil, nil)

	syncer.testStart()
	syncer.sync()
	rr, lr := remote.LastRound(), local.LastRound()
	require.Equal(t, rr, lr)
}

type periodicSyncLogger struct {
	logging.Logger
	WarnfCallback func(string, ...interface{})
}

func (cl *periodicSyncLogger) Warnf(s string, args ...interface{}) {
	// filter out few non-interesting warnings.
	switch s {
	case "fetchAndWrite(%v): lookback block doesn't exist, cannot authenticate new block":
		return
	case "fetchAndWrite(%v): cert did not authenticate block (attempt %d): %v":
		return
	}
	cl.Logger.Warnf(s, args...)
}

type periodicSyncDebugLogger struct {
	periodicSyncLogger
	debugMsgFilter []string
	debugMsgs      atomic.Uint32
}

func (cl *periodicSyncDebugLogger) Debugf(s string, args ...interface{}) {
	// save debug messages for later inspection.
	if len(cl.debugMsgFilter) > 0 {
		for _, filter := range cl.debugMsgFilter {
			if strings.Contains(s, filter) {
				cl.debugMsgs.Add(1)
				break
			}
		}
	} else {
		cl.debugMsgs.Add(1)
	}
	cl.Logger.Debugf(s, args...)
}

func TestSyncRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, 10)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	auth := &mockedAuthenticator{fail: true}
	initialLocalRound := local.LastRound()
	require.Zero(t, initialLocalRound)

	// Make Service
	localCfg := config.GetDefaultLocal()
	s := MakeService(logging.Base(), localCfg, net, local, auth, nil, nil)
	s.log = &periodicSyncLogger{Logger: logging.Base()}
	s.roundTimeEstimate = 2 * time.Second

	// Set disable round success
	err = s.SetDisableSyncRound(3)
	require.NoError(t, err)

	s.Start()
	defer s.Stop()
	// wait past the initial sync - which is known to fail due to the above "auth"
	time.Sleep(s.roundTimeEstimate*2 - 200*time.Millisecond)
	require.Equal(t, initialLocalRound, local.LastRound())
	auth.alter(-1, false)

	// wait until the catchup is done. Since we've might have missed the sleep window, we need to wait
	// until the synchronization is complete.
	waitStart := time.Now()
	for time.Since(waitStart) < 2*s.roundTimeEstimate {
		if remote.LastRound() == local.LastRound() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Assert that the last block is the one we expect--i.e. disableSyncRound - 1
	rnd := s.GetDisableSyncRound()
	rr, lr := rnd-1, local.LastRound()
	require.Equal(t, rr, lr)

	for r := basics.Round(1); r < rr; r++ {
		localBlock, err := local.Block(r)
		require.NoError(t, err)
		remoteBlock, err := remote.Block(r)
		require.NoError(t, err)
		require.Equal(t, remoteBlock.Hash(), localBlock.Hash())
	}

	// unset syncRound and make sure we finish catching up
	s.UnsetDisableSyncRound()
	// wait until the catchup is done
	waitStart = time.Now()
	for time.Since(waitStart) < 8*s.roundTimeEstimate {
		if remote.LastRound() == local.LastRound() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rr, lr = remote.LastRound(), local.LastRound()
	require.Equal(t, rr, lr)
	for r := basics.Round(1); r < remote.LastRound(); r++ {
		localBlock, err := local.Block(r)
		require.NoError(t, err)
		remoteBlock, err := remote.Block(r)
		require.NoError(t, err)
		require.Equal(t, remoteBlock.Hash(), localBlock.Hash())
	}
}

func TestPeriodicSync(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, 10)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	auth := &mockedAuthenticator{fail: true}
	initialLocalRound := local.LastRound()
	require.Zero(t, initialLocalRound)

	// Make Service
	s := MakeService(logging.Base(), defaultConfig, net, local, auth, nil, nil)
	s.log = &periodicSyncLogger{Logger: logging.Base()}
	s.roundTimeEstimate = 2 * time.Second

	s.Start()
	defer s.Stop()
	// wait past the initial sync - which is known to fail due to the above "auth"
	time.Sleep(s.roundTimeEstimate*2 - 200*time.Millisecond)
	require.Equal(t, initialLocalRound, local.LastRound())
	auth.alter(-1, false)

	// wait until the catchup is done. Since we've might have missed the sleep window, we need to wait
	// until the synchronization is complete.
	waitStart := time.Now()
	for time.Since(waitStart) < 10*s.roundTimeEstimate {
		if remote.LastRound() == local.LastRound() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Asserts that the last block is the one we expect
	rr, lr := remote.LastRound(), local.LastRound()
	require.Equal(t, rr, lr)

	for r := basics.Round(1); r < remote.LastRound(); r++ {
		localBlock, err := local.Block(r)
		require.NoError(t, err)
		remoteBlock, err := remote.Block(r)
		require.NoError(t, err)
		require.Equal(t, remoteBlock.Hash(), localBlock.Hash())
	}
}

func TestServiceFetchBlocksOneBlock(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	const numBlocks = 10
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})
	lastRoundAtStart := local.LastRound()

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, numBlocks-1)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	s := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: -1}, nil, nil)

	// Get last round

	// Start the service ( dummy )
	s.testStart()

	// Fetch blocks
	s.sync()

	// Asserts that the last block is the one we expect
	require.Equal(t, lastRoundAtStart+numBlocks, local.LastRound())

	// Get the same block we wrote
	block, _, _, err := makeUniversalBlockFetcher(logging.Base(),
		net,
		defaultConfig).fetchBlock(context.Background(), lastRoundAtStart+1, net.peers[0])

	require.NoError(t, err)

	//Check we wrote the correct block
	localBlock, err := local.Block(lastRoundAtStart + 1)
	require.NoError(t, err)
	require.Equal(t, *block, localBlock)
}

// TestAbruptWrites emulates the fact that the agreement can also generate new rounds
// When caught up, and the agreement service is taking the lead, the sync() stops and
// yields to the agreement. Agreement is emulated by the go func() loop in the test
func TestAbruptWrites(t *testing.T) {
	partitiontest.PartitionTest(t)

	numberOfBlocks := basics.Round(100)

	if testing.Short() {
		numberOfBlocks = 10
	}

	// Make Ledger
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	lastRound := local.LastRound()

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, numberOfBlocks-1)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	s := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: -1}, nil, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := lastRound + 1; i <= numberOfBlocks; i++ {
			time.Sleep(time.Duration(rand.Uint32()%5) * time.Millisecond)
			blk, err := remote.Block(i)
			require.NoError(t, err)
			var cert agreement.Certificate
			cert.Proposal.BlockDigest = blk.Digest()
			err = local.AddBlock(blk, cert)
			require.NoError(t, err)
		}
	}()

	// Start the service ( dummy )
	s.testStart()

	s.sync()
	wg.Wait()
	require.Equal(t, remote.LastRound(), local.LastRound())
}

func TestServiceFetchBlocksMultiBlocks(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	numberOfBlocks := basics.Round(100)
	if testing.Short() {
		numberOfBlocks = basics.Round(10)
	}
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	lastRoundAtStart := local.LastRound()

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, numberOfBlocks-1)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	syncer := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: -1}, nil, nil)
	fetcher := makeUniversalBlockFetcher(logging.Base(), net, defaultConfig)

	// Start the service ( dummy )
	syncer.testStart()

	// Fetch blocks
	syncer.sync()

	// Asserts that the last block is the one we expect
	require.Equal(t, lastRoundAtStart+numberOfBlocks, local.LastRound())

	for i := basics.Round(1); i <= numberOfBlocks; i++ {
		// Get the same block we wrote
		blk, _, _, err2 := fetcher.fetchBlock(context.Background(), i, net.GetPeers()[0])
		require.NoError(t, err2)

		// Check we wrote the correct block
		localBlock, err := local.Block(i)
		require.NoError(t, err)
		require.Equal(t, *blk, localBlock)
	}
}

func TestServiceFetchBlocksMalformed(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	const numBlocks = 10
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	lastRoundAtStart := local.LastRound()

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	addBlocks(t, remote, blk, numBlocks-1)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	s := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: int(lastRoundAtStart + 1)}, nil, nil)
	s.log = &periodicSyncLogger{Logger: logging.Base()}

	// Start the service ( dummy )
	s.testStart()

	s.sync()
	require.Equal(t, lastRoundAtStart, local.LastRound())
	// maybe check all peers/clients are closed here?
	//require.True(t, s.fetcherFactory.(*MockedFetcherFactory).fetcher.client.closed)
}

// Test the interruption in the initial loop
// This cannot happen in practice, but is used to test the code.
func TestOnSwitchToUnSupportedProtocol1(t *testing.T) {
	partitiontest.PartitionTest(t)

	lastRoundRemote := 5
	lastRoundLocal := 0
	roundWithSwitchOn := 0
	local, remote := helperTestOnSwitchToUnSupportedProtocol(t, lastRoundRemote, lastRoundLocal, roundWithSwitchOn, 0)

	// Last supported round is 0, but is guaranteed
	// to stop after 2 rounds.

	// SeedLookback is 2, which allows two parallel fetches.
	// i.e. rounds 1 and 2 may be simultaneously fetched.
	require.Less(t, int(local.LastRound()), 3)
	require.Equal(t, lastRoundRemote, int(remote.LastRound()))
	remote.Ledger.Close()
}

// Test the interruption in "the rest" loop
func TestOnSwitchToUnSupportedProtocol2(t *testing.T) {
	partitiontest.PartitionTest(t)

	lastRoundRemote := 10
	lastRoundLocal := 7
	roundWithSwitchOn := 5
	local, remote := helperTestOnSwitchToUnSupportedProtocol(t, lastRoundRemote, lastRoundLocal, roundWithSwitchOn, 0)
	for r := 1; r <= lastRoundLocal; r++ {
		blk, err := local.Block(basics.Round(r))
		require.NoError(t, err)
		require.Equal(t, r, int(blk.Round()))
	}
	require.Equal(t, lastRoundLocal, int(local.LastRound()))
	require.Equal(t, lastRoundRemote, int(remote.LastRound()))
	remote.Ledger.Close()
}

// Test the interruption with short notice (less than
// SeedLookback or the number of parallel fetches which in the
// test is the same: 2)
// This can not happen in practice, because there will be
// enough rounds for the protocol upgrade notice.
func TestOnSwitchToUnSupportedProtocol3(t *testing.T) {
	partitiontest.PartitionTest(t)

	lastRoundRemote := 14
	lastRoundLocal := 7
	roundWithSwitchOn := 7
	local, remote := helperTestOnSwitchToUnSupportedProtocol(t, lastRoundRemote, lastRoundLocal, roundWithSwitchOn, 0)
	for r := 1; r <= lastRoundLocal; r = r + 1 {
		blk, err := local.Block(basics.Round(r))
		require.NoError(t, err)
		require.Equal(t, r, int(blk.Round()))
	}
	// Since round with switch on (7) can be fetched
	// Simultaneously with round 8, round 8 might also be
	// fetched.
	require.Less(t, int(local.LastRound()), lastRoundLocal+2)
	require.Equal(t, lastRoundRemote, int(remote.LastRound()))
	remote.Ledger.Close()
}

// Test the interruption with short notice (less than
// SeedLookback or the number of parallel fetches which in the
// test is the same: 2)
// This case is a variation of the previous case. This may
// happen when the catchup service restart at the round when
// an upgrade happens.
func TestOnSwitchToUnSupportedProtocol4(t *testing.T) {
	partitiontest.PartitionTest(t)

	lastRoundRemote := 14
	lastRoundLocal := 7
	roundWithSwitchOn := 7
	roundsAlreadyInLocal := 8 // round 0 -> 7

	local, remote := helperTestOnSwitchToUnSupportedProtocol(
		t,
		lastRoundRemote,
		lastRoundLocal,
		roundWithSwitchOn,
		roundsAlreadyInLocal)

	for r := 1; r <= lastRoundLocal; r = r + 1 {
		blk, err := local.Block(basics.Round(r))
		require.NoError(t, err)
		require.Equal(t, r, int(blk.Round()))
	}
	// Since round with switch on (7) is already in the
	// ledger, round 8 will not be fetched.
	require.Equal(t, int(local.LastRound()), lastRoundLocal)
	require.Equal(t, lastRoundRemote, int(remote.LastRound()))
	remote.Ledger.Close()
}

func helperTestOnSwitchToUnSupportedProtocol(
	t *testing.T,
	lastRoundRemote,
	lastRoundLocal,
	roundWithSwitchOn,
	roundsToCopy int) (Ledger, *data.Ledger) {

	// Make Ledger
	mRemote, mLocal := testingenvWithUpgrade(t, lastRoundRemote, roundWithSwitchOn, lastRoundLocal+1)

	// Copy rounds to local
	for r := 1; r < roundsToCopy; r++ {
		mLocal.blocks = append(mLocal.blocks, mRemote.blocks[r])
	}

	local := mLocal

	config := defaultConfig
	config.CatchupParallelBlocks = 2

	block1 := mRemote.blocks[1]
	remote, _, blk, err := buildTestLedger(t, block1)
	if err != nil {
		t.Fatal(err)
		return local, remote
	}
	for i := 1; i < lastRoundRemote; i++ {
		blk.NextProtocolSwitchOn = mRemote.blocks[i+1].NextProtocolSwitchOn
		blk.NextProtocol = mRemote.blocks[i+1].NextProtocol
		// Adds blk.BlockHeader.Round + 1
		addBlocks(t, remote, blk, 1)
		blk.BlockHeader.Round++
	}

	// Create a network and block service
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), config, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	s := MakeService(logging.Base(), config, net, local, &mockedAuthenticator{errorRound: -1}, nil, nil)
	s.roundTimeEstimate = 2 * time.Second
	s.Start()
	defer s.Stop()

	s.workers.Wait()
	return local, remote
}

const defaultRewardUnit = 1e6

type mockedLedger struct {
	mu     deadlock.Mutex
	blocks []bookkeeping.Block
	chans  map[basics.Round]chan struct{}
}

func (m *mockedLedger) NextRound() basics.Round {
	return m.LastRound() + 1
}

func (m *mockedLedger) LastRound() basics.Round {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRound()
}

func (m *mockedLedger) lastRound() basics.Round {
	return m.blocks[len(m.blocks)-1].Round()
}

func (m *mockedLedger) AddBlock(blk bookkeeping.Block, cert agreement.Certificate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lastRound := m.lastRound()

	if blk.Round() > lastRound+1 {
		return errors.New("mockedLedger.AddBlock: bad block round provided")
	}

	if blk.Round() < lastRound+1 {
		return nil
	}

	m.blocks = append(m.blocks, blk)
	for r, ch := range m.chans {
		if r <= blk.Round() {
			close(ch)
			delete(m.chans, r)
		}
	}
	return nil
}

func (m *mockedLedger) Validate(ctx context.Context, blk bookkeeping.Block, executionPool execpool.BacklogPool) (*ledgercore.ValidatedBlock, error) {
	return nil, nil
}

func (m *mockedLedger) AddValidatedBlock(vb ledgercore.ValidatedBlock, cert agreement.Certificate) error {
	return nil
}

func (m *mockedLedger) ConsensusParams(r basics.Round) (config.ConsensusParams, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return config.Consensus[protocol.ConsensusCurrentVersion], nil
}

func (m *mockedLedger) Wait(r basics.Round) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	lastRound := m.lastRound()
	if lastRound >= r {
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	if m.chans == nil {
		m.chans = make(map[basics.Round]chan struct{})
	}
	if _, ok := m.chans[r]; !ok {
		ch := make(chan struct{})
		m.chans[r] = ch
	}
	return m.chans[r]
}

func (m *mockedLedger) WaitMem(r basics.Round) chan struct{} {
	return m.Wait(r)
}

func (m *mockedLedger) Block(r basics.Round) (bookkeeping.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r > m.lastRound() {
		return bookkeeping.Block{}, errors.New("mockedLedger.Block: round too high")
	}
	return m.blocks[r], nil
}

func (m *mockedLedger) BlockHdr(r basics.Round) (bookkeeping.BlockHeader, error) {
	blk, err := m.Block(r)
	return blk.BlockHeader, err
}

func (m *mockedLedger) Lookup(basics.Round, basics.Address) (basics.AccountData, error) {
	return basics.AccountData{}, errors.New("not needed for mockedLedger")
}
func (m *mockedLedger) Circulation(basics.Round, basics.Round) (basics.MicroAlgos, error) {
	return basics.MicroAlgos{}, errors.New("not needed for mockedLedger")
}
func (m *mockedLedger) ConsensusVersion(basics.Round) (protocol.ConsensusVersion, error) {
	return protocol.ConsensusCurrentVersion, nil
}
func (m *mockedLedger) EnsureBlock(block *bookkeeping.Block, c agreement.Certificate) {
	m.AddBlock(*block, c)
}
func (m *mockedLedger) Seed(basics.Round) (committee.Seed, error) {
	return committee.Seed{}, errors.New("not needed for mockedLedger")
}

func (m *mockedLedger) LookupDigest(basics.Round) (crypto.Digest, error) {
	return crypto.Digest{}, errors.New("not needed for mockedLedger")
}

func (m *mockedLedger) LookupAgreement(basics.Round, basics.Address) (basics.OnlineAccountData, error) {
	return basics.OnlineAccountData{}, errors.New("not needed for mockedLedger")
}

func (m *mockedLedger) IsWritingCatchpointDataFile() bool {
	return false
}

func (m *mockedLedger) IsBehindCommittingDeltas() bool {
	return false
}

type mockedBehindDeltasLedger struct {
	mockedLedger
}

func (m *mockedBehindDeltasLedger) IsBehindCommittingDeltas() bool {
	return true
}

func testingenvWithUpgrade(
	t testing.TB,
	numBlocks,
	roundWithSwitchOn,
	upgradeRound int) (ledger, emptyLedger *mockedLedger) {

	mLedger := new(mockedLedger)
	mEmptyLedger := new(mockedLedger)

	var blk bookkeeping.Block
	blk.CurrentProtocol = protocol.ConsensusCurrentVersion
	mLedger.blocks = append(mLedger.blocks, blk)
	mEmptyLedger.blocks = append(mEmptyLedger.blocks, blk)

	for i := 1; i <= numBlocks; i++ {
		blk = bookkeeping.MakeBlock(blk.BlockHeader)
		if roundWithSwitchOn <= i {
			modifierBlk := blk
			blkh := &modifierBlk.BlockHeader
			blkh.NextProtocolSwitchOn = basics.Round(upgradeRound)
			blkh.NextProtocol = protocol.ConsensusVersion("some-unsupported-protocol")

			mLedger.blocks = append(mLedger.blocks, modifierBlk)
			continue
		}

		mLedger.blocks = append(mLedger.blocks, blk)
	}

	return mLedger, mEmptyLedger
}

type MockVoteVerifier struct{}

func (avv *MockVoteVerifier) Quit() {
}
func (avv *MockVoteVerifier) Parallelism() int {
	return 1
}

// Start the catchup service, without starting the periodic sync.
func (s *Service) testStart() {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.InitialSyncDone = make(chan struct{})
}

func TestCatchupUnmatchedCertificate(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	const numBlocks = 10
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})
	lastRoundAtStart := local.LastRound()

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	defer remote.Close()
	addBlocks(t, remote, blk, numBlocks-1)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	// Make Service
	s := MakeService(logging.Base(), defaultConfig, net, local, &mockedAuthenticator{errorRound: int(lastRoundAtStart + 1)}, nil, nil)
	s.testStart()
	for roundNumber := 2; roundNumber < 10; roundNumber += 3 {
		pc := &PendingUnmatchedCertificate{
			Cert: agreement.Certificate{
				Round: basics.Round(roundNumber),
			},
			VoteVerifier: agreement.MakeAsyncVoteVerifier(nil),
		}
		block, _ := remote.Block(basics.Round(roundNumber))
		pc.Cert.Proposal.BlockDigest = block.Digest()
		s.syncCert(pc)
	}
}

// TestCreatePeerSelector tests if the correct peer selector configurations are prepared
func TestCreatePeerSelector(t *testing.T) {
	partitiontest.PartitionTest(t)

	s := MakeService(logging.Base(), defaultConfig, &httpTestPeerSource{}, new(mockedLedger), &mockedAuthenticator{errorRound: int(0 + 1)}, nil, nil)
	ps := createPeerSelector(s.net)

	cps, ok := ps.(*classBasedPeerSelector)
	require.True(t, ok)

	require.Equal(t, 4, len(cps.peerSelectors))

	require.Equal(t, network.PeersConnectedOut, cps.peerSelectors[0].peerClass)
	require.Equal(t, network.PeersPhonebookRelays, cps.peerSelectors[1].peerClass)
	require.Equal(t, network.PeersPhonebookArchivalNodes, cps.peerSelectors[2].peerClass)
	require.Equal(t, network.PeersConnectedIn, cps.peerSelectors[3].peerClass)

	require.Equal(t, 3, cps.peerSelectors[0].toleranceFactor)
	require.Equal(t, 3, cps.peerSelectors[1].toleranceFactor)
	require.Equal(t, 10, cps.peerSelectors[2].toleranceFactor)
	require.Equal(t, 3, cps.peerSelectors[3].toleranceFactor)
}

func TestServiceStartStop(t *testing.T) {
	partitiontest.PartitionTest(t)

	cfg := defaultConfig
	ledger := new(mockedLedger)
	ledger.blocks = append(ledger.blocks, bookkeeping.Block{})
	s := MakeService(logging.Base(), cfg, &httpTestPeerSource{}, ledger, &mockedAuthenticator{errorRound: int(0 + 1)}, nil, nil)
	s.Start()
	s.Stop()

	// The test ensures that Stop() returns and does not block forever.
}

func TestSynchronizingTime(t *testing.T) {
	partitiontest.PartitionTest(t)

	cfg := defaultConfig
	ledger := new(mockedLedger)
	ledger.blocks = append(ledger.blocks, bookkeeping.Block{})
	s := MakeService(logging.Base(), cfg, &httpTestPeerSource{}, ledger, &mockedAuthenticator{errorRound: int(0 + 1)}, nil, nil)

	require.Equal(t, time.Duration(0), s.SynchronizingTime())
	s.syncStartNS.Store(1000000)
	require.NotEqual(t, time.Duration(0), s.SynchronizingTime())
}

func TestDownloadBlocksToSupportStateProofs(t *testing.T) {
	partitiontest.PartitionTest(t)

	// make sure we download enough blocks to verify state proof 512
	topBlk := bookkeeping.Block{}
	topBlk.BlockHeader.Round = 1500
	topBlk.BlockHeader.CurrentProtocol = protocol.ConsensusCurrentVersion
	trackingData := bookkeeping.StateProofTrackingData{StateProofNextRound: 512}
	topBlk.BlockHeader.StateProofTracking = make(map[protocol.StateProofType]bookkeeping.StateProofTrackingData)
	topBlk.BlockHeader.StateProofTracking[protocol.StateProofBasic] = trackingData

	lookback := lookbackForStateproofsSupport(&topBlk)
	oldestRound := topBlk.BlockHeader.Round.SubSaturate(basics.Round(lookback))
	assert.Equal(t, uint64(oldestRound), 512-config.Consensus[protocol.ConsensusCurrentVersion].StateProofInterval-config.Consensus[protocol.ConsensusCurrentVersion].StateProofVotersLookback)

	// the network has made progress and now it is on round 8000. in this case we would not download blocks to cover 512.
	// instead, we will download blocks to confirm only the recovery period lookback.
	topBlk = bookkeeping.Block{}
	topBlk.BlockHeader.Round = 8000
	topBlk.BlockHeader.CurrentProtocol = protocol.ConsensusCurrentVersion
	trackingData = bookkeeping.StateProofTrackingData{StateProofNextRound: 512}
	topBlk.BlockHeader.StateProofTracking = make(map[protocol.StateProofType]bookkeeping.StateProofTrackingData)
	topBlk.BlockHeader.StateProofTracking[protocol.StateProofBasic] = trackingData

	lookback = lookbackForStateproofsSupport(&topBlk)
	oldestRound = topBlk.BlockHeader.Round.SubSaturate(basics.Round(lookback))

	lowestRoundToRetain := 8000 - (8000 % config.Consensus[protocol.ConsensusCurrentVersion].StateProofInterval) -
		config.Consensus[protocol.ConsensusCurrentVersion].StateProofInterval*(config.Consensus[protocol.ConsensusCurrentVersion].StateProofMaxRecoveryIntervals+1) - config.Consensus[protocol.ConsensusCurrentVersion].StateProofVotersLookback

	assert.Equal(t, uint64(oldestRound), lowestRoundToRetain)

	topBlk = bookkeeping.Block{}
	topBlk.BlockHeader.Round = 8000
	topBlk.BlockHeader.CurrentProtocol = protocol.ConsensusV32
	lookback = lookbackForStateproofsSupport(&topBlk)
	assert.Equal(t, uint64(0), lookback)
}

// TestServiceLedgerUnavailable checks a local ledger that is unavailable cannot catchup up to remote round
func TestServiceLedgerUnavailable(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	local := new(mockedBehindDeltasLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	const numBlocks = 10
	addBlocks(t, remote, blk, numBlocks)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	require.Equal(t, basics.Round(0), local.LastRound())
	require.Equal(t, basics.Round(numBlocks+1), remote.LastRound())

	// Make Service
	auth := &mockedAuthenticator{fail: false}
	cfg := config.GetDefaultLocal()
	cfg.CatchupParallelBlocks = 2
	s := MakeService(logging.Base(), cfg, net, local, auth, nil, nil)
	s.log = &periodicSyncLogger{Logger: logging.Base()}
	s.roundTimeEstimate = 2 * time.Second

	s.testStart()
	defer s.Stop()
	s.sync()
	require.Greater(t, local.LastRound(), basics.Round(0))
	require.Less(t, local.LastRound(), remote.LastRound())
}

// TestServiceNoBlockForRound checks if fetchAndWrite does not repeats 500 times if a block not avaialble
func TestServiceNoBlockForRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	// Make Ledger
	local := new(mockedLedger)
	local.blocks = append(local.blocks, bookkeeping.Block{})

	remote, _, blk, err := buildTestLedger(t, bookkeeping.Block{})
	if err != nil {
		t.Fatal(err)
		return
	}
	const numBlocks = 10
	addBlocks(t, remote, blk, numBlocks)

	// Create a network and block service
	blockServiceConfig := config.GetDefaultLocal()
	net := &httpTestPeerSource{}
	ls := rpcs.MakeBlockService(logging.Base(), blockServiceConfig, remote, net, "test genesisID")

	nodeA := basicRPCNode{}
	ls.RegisterHandlers(&nodeA)
	nodeA.start()
	defer nodeA.stop()
	rootURL := nodeA.rootURL()
	net.addPeer(rootURL)

	require.Equal(t, basics.Round(0), local.LastRound())
	require.Equal(t, basics.Round(numBlocks+1), remote.LastRound())

	// Make Service
	auth := &mockedAuthenticator{fail: false}
	cfg := config.GetDefaultLocal()
	cfg.CatchupParallelBlocks = 8
	s := MakeService(logging.Base(), cfg, net, local, auth, nil, nil)
	pl := &periodicSyncDebugLogger{periodicSyncLogger: periodicSyncLogger{Logger: logging.Base()}}
	s.log = pl
	s.roundTimeEstimate = 1 * time.Second

	s.testStart()
	defer s.Stop()
	s.sync()

	// without the fix there are about 2k messages (4x catchupRetryLimit)
	// with the fix expect less than catchupRetryLimit
	require.Less(t, int(pl.debugMsgs.Load()), catchupRetryLimit)
}
