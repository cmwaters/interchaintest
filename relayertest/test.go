// Package relayertest exposes a TestRelayer function
// that can be imported into other packages' tests.
//
// The exported TestRelayer function is intended to be a stable API
// that calls the other TestRelayer_* functions, which are not guaranteed
// to remain a stable API.
//
// External packages that intend to run IBC tests against their relayer implementation
// should define their own implementation of ibc.RelayerFactory,
// and in most cases should use an instance of ibc.BuiltinChainFactory.
//
//     package myrelayer_test
//
//     import (
//       "testing"
//
//       "github.com/strangelove-ventures/ibc-test-framework/ibc"
//       "github.com/strangelove-ventures/ibc-test-framework/relayertest"
//     )
//
//     func TestMyRelayer(t *testing.T) {
//       relayertest.TestRelayer(t, ibc.NewBuiltinChainFactory([]ibc.BuiltinChainFactoryEntry{
//         {Name: "foo_bar" /* ... */},
//       }, MyRelayerFactory())
//     }
//
// Although the relayertest package is made available as a convenience for other projects,
// the ibc-test-framework project should be considered the canonical definition of tests and configuration.
package relayertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	"github.com/strangelove-ventures/ibc-test-framework/ibc"
	"github.com/strangelove-ventures/ibc-test-framework/ibctest"
	"github.com/strangelove-ventures/ibc-test-framework/relayer"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

const (
	userFaucetFund = int64(10_000_000_000)
	testCoinAmount = int64(1_000_000)
)

type RelayerTestCase struct {
	Config RelayerTestCaseConfig
	// user on source chain
	Users []*ibctest.User
	// temp storage in between test phases
	Cache []string
}

type RelayerTestCaseConfig struct {
	Name string
	// which relayer capabilities are required to run this test
	RequiredRelayerCapabilities []relayer.Capability
	// function to run after the chains are started but before the relayer is started
	// e.g. send a transfer and wait for it to timeout so that the relayer will handle it once it is timed out
	PreRelayerStart func(context.Context, *testing.T, *RelayerTestCase, ibc.Chain, ibc.Chain, []ibc.ChannelOutput)
	// test after chains and relayers are started
	Test func(context.Context, *testing.T, *RelayerTestCase, ibc.Chain, ibc.Chain, []ibc.ChannelOutput)
}

var relayerTestCaseConfigs = [...]RelayerTestCaseConfig{
	{
		Name:            "relay packet",
		PreRelayerStart: preRelayerStart_RelayPacket,
		Test:            testPacketRelaySuccess,
	},
	{
		Name:            "no timeout",
		PreRelayerStart: preRelayerStart_NoTimeout,
		Test:            testPacketRelaySuccess,
	},
	{
		Name:                        "height timeout",
		RequiredRelayerCapabilities: []relayer.Capability{relayer.HeightTimeout},
		PreRelayerStart:             preRelayerStart_HeightTimeout,
		Test:                        testPacketRelayFail,
	},
	{
		Name:                        "timestamp timeout",
		RequiredRelayerCapabilities: []relayer.Capability{relayer.TimestampTimeout},
		PreRelayerStart:             preRelayerStart_TimestampTimeout,
		Test:                        testPacketRelayFail,
	},
}

func requireCapabilities(t *testing.T, rf ibctest.RelayerFactory, reqCaps ...relayer.Capability) {
	t.Helper()

	missing := missingCapabilities(rf, reqCaps...)

	if len(missing) > 0 {
		t.Skipf("skipping due to missing capabilities +%s", missing)
	}
}

func missingCapabilities(rf ibctest.RelayerFactory, reqCaps ...relayer.Capability) []relayer.Capability {
	caps := rf.Capabilities()
	missing := []relayer.Capability{}
	for _, c := range reqCaps {
		if !caps[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

func sendIBCTransfersFromBothChainsWithTimeout(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
	timeout *ibc.IBCTimeout,
) {
	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]

	dstChainCfg := dstChain.Config()
	dstUser := testCase.Users[1]

	// will send ibc transfers from user wallet on both chains to their own respective wallet on the other chain

	testCoinSrcToDst := ibc.WalletAmount{
		Address: srcUser.Bech32Address(dstChainCfg.Bech32Prefix),
		Denom:   srcChainCfg.Denom,
		Amount:  testCoinAmount,
	}
	testCoinDstToSrc := ibc.WalletAmount{
		Address: dstUser.Bech32Address(srcChainCfg.Bech32Prefix),
		Denom:   dstChainCfg.Denom,
		Amount:  testCoinAmount,
	}

	var eg errgroup.Group
	var txHashSrc string
	var txHashDst string

	eg.Go(func() error {
		var err error
		txHashSrc, err = srcChain.SendIBCTransfer(ctx, channels[0].ChannelID, srcUser.KeyName, testCoinSrcToDst, timeout)
		if err != nil {
			return fmt.Errorf("failed to send ibc transfer from source: %v", err)
		}
		return nil
	})

	eg.Go(func() error {
		var err error
		txHashDst, err = dstChain.SendIBCTransfer(ctx, channels[0].Counterparty.ChannelID, dstUser.KeyName, testCoinDstToSrc, timeout)
		if err != nil {
			return fmt.Errorf("failed to send ibc transfer from source: %v", err)
		}
		return nil
	})

	require.NoError(t, eg.Wait())

	testCase.Cache = []string{txHashSrc, txHashDst}
}

// TestRelayer is the stable API exposed by the relayertest package.
// This is intended to be used by Go unit tests.
func TestRelayer(t *testing.T, cf ibctest.ChainFactory, rf ibctest.RelayerFactory) {
	ctx, home, pool, network, err := ibctest.SetupTestRun(t)
	require.NoError(t, err, "failed to set up test run")

	srcChain, dstChain, err := cf.Pair(t.Name())
	require.NoError(t, err, "failed to get chain pair")

	preRelayerStartFuncs := []func([]ibc.ChannelOutput){}

	testCases := []*RelayerTestCase{}

	for _, testCaseConfig := range relayerTestCaseConfigs {
		if len(missingCapabilities(rf, testCaseConfig.RequiredRelayerCapabilities...)) > 0 {
			continue
		}
		testCase := RelayerTestCase{
			Config: testCaseConfig,
		}
		testCases = append(testCases, &testCase)
		preRelayerStartFunc := func(channels []ibc.ChannelOutput) {

			// fund a user wallet on both chains, save on test case
			testCase.Users = ibctest.GetAndFundTestUsers(t, ctx, strings.ReplaceAll(testCase.Config.Name, " ", "-"), userFaucetFund, srcChain, dstChain)
			// run test specific pre relayer start action
			testCase.Config.PreRelayerStart(ctx, t, &testCase, srcChain, dstChain, channels)
		}
		preRelayerStartFuncs = append(preRelayerStartFuncs, preRelayerStartFunc)
	}

	// startup both chains and relayer
	// creates wallets in the relayer for src and dst chain
	// funds relayer src and dst wallets on respective chain in genesis
	// creates a faucet account on the both chains (separate fullnode)
	// funds faucet accounts in genesis
	_, channels, err := ibctest.StartChainsAndRelayerFromFactory(t, ctx, pool, network, home, srcChain, dstChain, rf, preRelayerStartFuncs)
	require.NoError(t, err, "failed to StartChainsAndRelayerFromFactory")

	// TODO poll for acks inside of each testCase `.Config.Test` method instead of just waiting for blocks here
	// Wait for both chains to produce 10 blocks per test case.
	// This is long to allow for intermittent retries inside the relayer.
	require.NoError(t, ibctest.WaitForBlocks(int64(10*len(testCases)), srcChain, dstChain), "failed to wait for blocks")

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.Config.Name, func(t *testing.T) {
			requireCapabilities(t, rf, testCase.Config.RequiredRelayerCapabilities...)
			t.Parallel()
			testCase.Config.Test(ctx, t, testCase, srcChain, dstChain, channels)
		})
	}
}

// PreRelayerStart methods for the RelayerTestCases

func preRelayerStart_RelayPacket(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, nil)
}

func preRelayerStart_NoTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutDisabled := ibc.IBCTimeout{Height: 0, NanoSeconds: 0}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutDisabled)
	// TODO should we wait here to make sure it successfully relays a packet beyond the default timeout period?
	// would need to shorten the chain default timeouts somehow to make that a feasible test
}

func preRelayerStart_HeightTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutHeight := ibc.IBCTimeout{Height: 10}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutHeight)
	// wait for both chains to produce 15 blocks to expire timeout
	require.NoError(t, ibctest.WaitForBlocks(15, srcChain, dstChain), "failed to wait for blocks")
}

func preRelayerStart_TimestampTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutTimestamp := ibc.IBCTimeout{NanoSeconds: uint64((10 * time.Second).Nanoseconds())}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutTimestamp)
	// wait for 15 seconds to expire timeout
	time.Sleep(15 * time.Second)
}

// Ensure that a queued packet is successfully relayed.
func testPacketRelaySuccess(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
) {
	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]
	srcDenom := srcChainCfg.Denom

	dstChainCfg := dstChain.Config()
	dstUser := testCase.Users[1]
	dstDenom := dstChainCfg.Denom

	// [BEGIN] assert on source to destination transfer
	// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
	srcInitialBalance := userFaucetFund
	dstInitialBalance := int64(0)

	// fetch src ibc transfer tx
	srcTxHash := testCase.Cache[0]
	srcTx, err := srcChain.GetTransaction(ctx, srcTxHash)
	require.NoError(t, err, "failed to get ibc transaction")

	// get ibc denom for src denom on dst chain
	srcDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[0].Counterparty.PortID, channels[0].Counterparty.ChannelID, srcDenom))
	dstIbcDenom := srcDenomTrace.IBCDenom()

	srcFinalBalance, err := srcChain.GetBalance(ctx, srcUser.Bech32Address(srcChainCfg.Bech32Prefix), srcDenom)
	require.NoError(t, err, "failed to get balance from source chain")

	dstFinalBalance, err := dstChain.GetBalance(ctx, srcUser.Bech32Address(dstChainCfg.Bech32Prefix), dstIbcDenom)
	require.NoError(t, err, "failed to get balance from dest chain")

	totalFees := srcChain.GetGasFeesInNativeDenom(srcTx.GasWanted)
	expectedDifference := testCoinAmount + totalFees

	require.Equal(t, srcInitialBalance-expectedDifference, srcFinalBalance)
	require.Equal(t, dstInitialBalance+testCoinAmount, dstFinalBalance)
	// [END] assert on source to destination transfer

	// [BEGIN] assert on destination to source transfer
	// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
	srcInitialBalance = int64(0)
	dstInitialBalance = userFaucetFund
	// fetch src ibc transfer tx
	dstTxHash := testCase.Cache[1]
	dstTx, err := dstChain.GetTransaction(ctx, dstTxHash)
	require.NoError(t, err, "failed to get ibc transaction")

	// get ibc denom for dst denom on src chain
	dstDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[0].PortID, channels[0].ChannelID, dstDenom))
	srcIbcDenom := dstDenomTrace.IBCDenom()

	srcFinalBalance, err = srcChain.GetBalance(ctx, dstUser.Bech32Address(srcChainCfg.Bech32Prefix), srcIbcDenom)
	require.NoError(t, err, "failed to get balance from source chain")

	dstFinalBalance, err = dstChain.GetBalance(ctx, dstUser.Bech32Address(dstChainCfg.Bech32Prefix), dstDenom)
	require.NoError(t, err, "failed to get balance from dest chain")

	totalFees = dstChain.GetGasFeesInNativeDenom(dstTx.GasWanted)
	expectedDifference = testCoinAmount + totalFees

	require.Equal(t, srcInitialBalance+testCoinAmount, srcFinalBalance)
	require.Equal(t, dstInitialBalance-expectedDifference, dstFinalBalance)
	// [END] assert on destination to source transfer
}

// Ensure that a queued packet that should not be relayed is not relayed.
func testPacketRelayFail(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
) {
	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]
	srcDenom := srcChainCfg.Denom

	dstChainCfg := dstChain.Config()
	dstUser := testCase.Users[1]
	dstDenom := dstChainCfg.Denom

	// [BEGIN] assert on source to destination transfer
	// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
	srcInitialBalance := userFaucetFund
	dstInitialBalance := int64(0)

	// fetch src ibc transfer tx
	srcTxHash := testCase.Cache[0]
	srcTx, err := srcChain.GetTransaction(ctx, srcTxHash)
	require.NoError(t, err, "failed to get ibc transaction")

	// get ibc denom for src denom on dst chain
	srcDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[0].Counterparty.PortID, channels[0].Counterparty.ChannelID, srcDenom))
	dstIbcDenom := srcDenomTrace.IBCDenom()

	srcFinalBalance, err := srcChain.GetBalance(ctx, srcUser.Bech32Address(srcChainCfg.Bech32Prefix), srcDenom)
	require.NoError(t, err, "failed to get balance from source chain")

	dstFinalBalance, err := dstChain.GetBalance(ctx, srcUser.Bech32Address(dstChainCfg.Bech32Prefix), dstIbcDenom)
	require.NoError(t, err, "failed to get balance from dest chain")

	totalFees := srcChain.GetGasFeesInNativeDenom(srcTx.GasWanted)

	require.Equal(t, srcInitialBalance-totalFees, srcFinalBalance)
	require.Equal(t, dstInitialBalance, dstFinalBalance)
	// [END] assert on source to destination transfer

	// [BEGIN] assert on destination to source transfer
	// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
	srcInitialBalance = int64(0)
	dstInitialBalance = userFaucetFund
	// fetch src ibc transfer tx
	dstTxHash := testCase.Cache[1]
	dstTx, err := dstChain.GetTransaction(ctx, dstTxHash)
	require.NoError(t, err, "failed to get ibc transaction")

	// get ibc denom for dst denom on src chain
	dstDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[0].PortID, channels[0].ChannelID, dstDenom))
	srcIbcDenom := dstDenomTrace.IBCDenom()

	srcFinalBalance, err = srcChain.GetBalance(ctx, dstUser.Bech32Address(srcChainCfg.Bech32Prefix), srcIbcDenom)
	require.NoError(t, err, "failed to get balance from source chain")

	dstFinalBalance, err = dstChain.GetBalance(ctx, dstUser.Bech32Address(dstChainCfg.Bech32Prefix), dstDenom)
	require.NoError(t, err, "failed to get balance from dest chain")

	totalFees = dstChain.GetGasFeesInNativeDenom(dstTx.GasWanted)

	require.Equal(t, srcInitialBalance, srcFinalBalance)
	require.Equal(t, dstInitialBalance-totalFees, dstFinalBalance)
	// [END] assert on destination to source transfer
}