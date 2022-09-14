package stride_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	"github.com/strangelove-ventures/ibctest/v3"
	"github.com/strangelove-ventures/ibctest/v3/chain/cosmos"
	"github.com/strangelove-ventures/ibctest/v3/examples/stride"
	"github.com/strangelove-ventures/ibctest/v3/ibc"
	"github.com/strangelove-ventures/ibctest/v3/internal/dockerutil"
	"github.com/strangelove-ventures/ibctest/v3/relayer"
	"github.com/strangelove-ventures/ibctest/v3/test"
	"github.com/strangelove-ventures/ibctest/v3/testreporter"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

// TestStrideICAandICQ is a test case that performs simulations and assertions around interchain accounts
// and the client implementation of interchain queries. See: https://github.com/Stride-Labs/interchain-queries
func TestStrideICAandICQ(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	client, network := ibctest.DockerSetup(t)

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	ctx := context.Background()

	// Define chains involved in test
	cf := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{
			Name:      "stride",
			ChainName: "stride",
			ChainConfig: ibc.ChainConfig{
				Type:    "cosmos",
				Name:    "stride",
				ChainID: "stride-1",
				Images: []ibc.DockerImage{{
					Repository: "ghcr.io/strangelove-ventures/heighliner/stride",
					Version:    "andrew-test_admin",
					UidGid:     dockerutil.GetHeighlinerUserString(),
				}},
				Bin:            "strided",
				Bech32Prefix:   "stride",
				Denom:          "ustrd",
				GasPrices:      "0.0ustrd",
				TrustingPeriod: TrustingPeriod,
				GasAdjustment:  1.1,
				ModifyGenesis:  ModifyGenesisStride(),
				EncodingConfig: stride.StrideEncoding(),
			}},
		{
			Name:      "gaia",
			ChainName: "gaia",
			Version:   "v7.0.3",
			ChainConfig: ibc.ChainConfig{
				ModifyGenesis:  ModifyGenesisStrideCounterparty(),
				TrustingPeriod: TrustingPeriod,
			},
		},
		{
			Name:      "osmosis",
			ChainName: "osmosis",
			Version:   "v12.0.0-rc1",
			ChainConfig: ibc.ChainConfig{
				ModifyGenesis:  ModifyGenesisStrideCounterparty(),
				TrustingPeriod: TrustingPeriod,
			},
		},
		{
			Name:      "juno",
			ChainName: "juno",
			Version:   "v10.0.0-alpha",
			ChainConfig: ibc.ChainConfig{
				ModifyGenesis:  ModifyGenesisStrideCounterparty(),
				TrustingPeriod: TrustingPeriod,
			},
		},
	})

	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	stride, gaia, osmosis, juno := chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain), chains[2].(*cosmos.CosmosChain), chains[3].(*cosmos.CosmosChain)
	strideCfg, gaiaCfg, osmosisCfg, junoCfg := stride.Config(), gaia.Config(), osmosis.Config(), juno.Config()

	// Get a relayer instance
	r := ibctest.NewBuiltinRelayerFactory(
		ibc.CosmosRly,
		zaptest.NewLogger(t),
		relayer.CustomDockerImage("ghcr.io/cosmos/relayer", "andrew-client_icq", "100:1000"),
		relayer.StartupFlags("-p", "events"),
	).Build(t, client, network)

	// Build the network; spin up the chains and configure the relayer
	const pathStrideGaia = "stride-gaia"
	const pathStrideOsmosis = "stride-osmosis"
	const pathStrideJuno = "stride-juno"
	const relayerName = "relayer"

	clientOpts := ibc.DefaultClientOpts()
	clientOpts.TrustingPeriod = TrustingPeriod

	ic := ibctest.NewInterchain().
		AddChain(stride).
		AddChain(gaia).
		AddChain(osmosis).
		AddChain(juno).
		AddRelayer(r, relayerName).
		AddLink(ibctest.InterchainLink{
			Chain1:           stride,
			Chain2:           gaia,
			Relayer:          r,
			Path:             pathStrideGaia,
			CreateClientOpts: clientOpts,
		}).
		AddLink(ibctest.InterchainLink{
			Chain1:           stride,
			Chain2:           osmosis,
			Relayer:          r,
			Path:             pathStrideOsmosis,
			CreateClientOpts: clientOpts,
		}).
		AddLink(ibctest.InterchainLink{
			Chain1:           stride,
			Chain2:           juno,
			Relayer:          r,
			Path:             pathStrideJuno,
			CreateClientOpts: clientOpts,
		})

	require.NoError(t, ic.Build(ctx, eRep, ibctest.InterchainBuildOptions{
		TestName:          t.Name(),
		Client:            client,
		NetworkID:         network,
		BlockDatabaseFile: ibctest.DefaultBlockDatabaseFilepath(),

		SkipPathCreation: false,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
	})

	// Fund user accounts, so we can query balances and make assertions.
	const userFunds = int64(10_000_000_000_000)
	users := ibctest.GetAndFundTestUsers(t, ctx, t.Name(), userFunds, stride, gaia, osmosis, juno)
	strideUser, gaiaUser, osmosisUser, junoUser := users[0], users[1], users[2], users[3]

	strideFullNode := stride.FullNodes[0]

	// Wait a few blocks for user accounts to be created on chain.
	err = test.WaitForBlocks(ctx, 2, stride, gaia /*, osmosis*/)
	require.NoError(t, err)

	// Start the relayers
	err = r.StartRelayer(ctx, eRep, pathStrideGaia)
	require.NoError(t, err)

	err = r.StartRelayer(ctx, eRep, pathStrideOsmosis)
	require.NoError(t, err)

	err = r.StartRelayer(ctx, eRep, pathStrideJuno)
	require.NoError(t, err)

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				t.Logf("an error occured while stopping the relayer: %s", err)
			}
		},
	)

	// Wait a few blocks for the relayer to start.
	err = test.WaitForBlocks(ctx, 2, stride, gaia, osmosis, juno)
	require.NoError(t, err)

	// Recover stride admin key
	err = stride.RecoverKey(ctx, StrideAdminAccount, StrideAdminMnemonic)
	require.NoError(t, err)

	strideAdminAddrBytes, err := stride.GetAddress(ctx, StrideAdminAccount)
	require.NoError(t, err)

	strideAdminAddr, err := types.Bech32ifyAddressBytes(strideCfg.Bech32Prefix, strideAdminAddrBytes)
	require.NoError(t, err)

	err = stride.SendFunds(ctx, ibctest.FaucetAccountKeyName, ibc.WalletAmount{
		Address: strideAdminAddr,
		Amount:  userFunds,
		Denom:   strideCfg.Denom,
	})
	require.NoError(t, err, "failed to fund stride admin account")

	// get native chain user addresses
	strideAddr := strideUser.Bech32Address(strideCfg.Bech32Prefix)
	require.NotEmpty(t, strideAddr)

	gaiaAddress := gaiaUser.Bech32Address(gaiaCfg.Bech32Prefix)
	require.NotEmpty(t, gaiaAddress)

	osmosisAddress := osmosisUser.Bech32Address(osmosisCfg.Bech32Prefix)
	require.NotEmpty(t, osmosisAddress)

	junoAddress := junoUser.Bech32Address(junoCfg.Bech32Prefix)
	require.NotEmpty(t, junoAddress)

	// get ibc paths
	gaiaConns, err := r.GetConnections(ctx, eRep, gaiaCfg.ChainID)
	require.NoError(t, err)

	gaiaChans, err := r.GetChannels(ctx, eRep, gaiaCfg.ChainID)
	require.NoError(t, err)

	osmosisConns, err := r.GetConnections(ctx, eRep, osmosisCfg.ChainID)
	require.NoError(t, err)

	osmosisChans, err := r.GetChannels(ctx, eRep, osmosisCfg.ChainID)
	require.NoError(t, err)

	junoConns, err := r.GetConnections(ctx, eRep, junoCfg.ChainID)
	require.NoError(t, err)

	junoChans, err := r.GetChannels(ctx, eRep, junoCfg.ChainID)
	require.NoError(t, err)

	atomIBCDenom := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(gaiaChans[0].Counterparty.PortID, gaiaChans[0].Counterparty.ChannelID, gaiaCfg.Denom)).IBCDenom()
	osmosisIBCDenom := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(osmosisChans[0].Counterparty.PortID, osmosisChans[0].Counterparty.ChannelID, osmosisCfg.Denom)).IBCDenom()
	junoIBCDenom := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(junoChans[0].Counterparty.PortID, junoChans[0].Counterparty.ChannelID, junoCfg.Denom)).IBCDenom()

	var eg errgroup.Group

	// Fund stride user with ibc transfers in parallel
	eg.Go(func() error {
		gaiaHeight, err := gaia.Height(ctx)
		if err != nil {
			return err
		}
		// Fund stride user with ibc denom atom
		tx, err := gaia.SendIBCTransfer(ctx, gaiaChans[0].ChannelID, gaiaUser.KeyName, ibc.WalletAmount{
			Amount:  1_000_000_000_000,
			Denom:   gaiaCfg.Denom,
			Address: strideAddr,
		}, nil)
		if err != nil {
			return err
		}
		_, err = test.PollForAck(ctx, gaia, gaiaHeight, gaiaHeight+10, tx.Packet)
		return err
	})

	eg.Go(func() error {
		osmosisHeight, err := osmosis.Height(ctx)
		if err != nil {
			return err
		}
		// Fund stride user with ibc denom osmo
		tx, err := osmosis.SendIBCTransfer(ctx, osmosisChans[0].ChannelID, osmosisUser.KeyName, ibc.WalletAmount{
			Amount:  1_000_000_000_000,
			Denom:   osmosisCfg.Denom,
			Address: strideAddr,
		}, nil)
		if err != nil {
			return err
		}
		_, err = test.PollForAck(ctx, osmosis, osmosisHeight, osmosisHeight+10, tx.Packet)
		return err

	})

	eg.Go(func() error {
		junoHeight, err := juno.Height(ctx)
		if err != nil {
			return err
		}
		// Fund stride user with ibc denom juno
		tx, err := juno.SendIBCTransfer(ctx, junoChans[0].ChannelID, junoUser.KeyName, ibc.WalletAmount{
			Amount:  1_000_000_000_000,
			Denom:   junoCfg.Denom,
			Address: strideAddr,
		}, nil)
		if err != nil {
			return err
		}
		_, err = test.PollForAck(ctx, juno, junoHeight, junoHeight+10, tx.Packet)
		return err
	})

	require.NoError(t, eg.Wait())

	// Register gaia host zone
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "register-host-zone",
		gaiaConns[0].Counterparty.ConnectionId, gaiaCfg.Denom, gaiaCfg.Bech32Prefix,
		atomIBCDenom, gaiaChans[0].Counterparty.ChannelID, "1",
		"--gas", "1000000",
	)
	require.NoError(t, err)

	// Register osmosis host zone
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "register-host-zone",
		osmosisConns[0].Counterparty.ConnectionId, osmosisCfg.Denom, osmosisCfg.Bech32Prefix,
		osmosisIBCDenom, osmosisChans[0].Counterparty.ChannelID, "1",
		"--gas", "1000000",
	)
	require.NoError(t, err)

	// Register juno host zone
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "register-host-zone",
		junoConns[0].Counterparty.ConnectionId, junoCfg.Denom, junoCfg.Bech32Prefix,
		junoIBCDenom, junoChans[0].Counterparty.ChannelID, "1",
		"--gas", "1000000",
	)
	require.NoError(t, err)

	// TODO: replace with poll for channel open confirm messages
	// Wait a few blocks for the ICA accounts to be setup
	err = test.WaitForBlocks(ctx, 15, stride, gaia)
	require.NoError(t, err)

	// Get validator addresses

	gaiaVal1Address, err := gaia.Validators[0].KeyBech32(ctx, "validator", "val")
	require.NoError(t, err)

	gaiaVal2Address, err := gaia.Validators[1].KeyBech32(ctx, "validator", "val")
	require.NoError(t, err)

	osmosisValAddress, err := osmosis.Validators[0].KeyBech32(ctx, "validator", "val")
	require.NoError(t, err)

	junoValAddress, err := juno.Validators[0].KeyBech32(ctx, "validator", "val")
	require.NoError(t, err)

	// Add gaia validator 1
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "add-validator",
		gaiaCfg.ChainID, "gval1", gaiaVal1Address,
		"10", "5",
	)
	require.NoError(t, err)

	// Add gaia validator 2
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "add-validator",
		gaiaCfg.ChainID, "gval2", gaiaVal2Address,
		"10", "10",
	)
	require.NoError(t, err)

	// Add osmosis validator
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "add-validator",
		osmosisCfg.ChainID, "oval1", osmosisValAddress,
		"10", "5",
	)
	require.NoError(t, err)

	// Add juno validator
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "add-validator",
		junoCfg.ChainID, "jval1", junoValAddress,
		"10", "5",
	)
	require.NoError(t, err)

	var gaiaHostZone, osmosisHostZone, junoHostZone HostZoneWrapper

	// query gaia host zone
	stdout, _, err := strideFullNode.ExecQuery(ctx,
		"stakeibc", "show-host-zone", gaiaCfg.ChainID,
	)
	require.NoError(t, err)
	err = json.Unmarshal(stdout, &gaiaHostZone)
	require.NoError(t, err)

	// query osmosis host zone
	stdout, _, err = strideFullNode.ExecQuery(ctx,
		"stakeibc", "show-host-zone", osmosisCfg.ChainID,
	)
	require.NoError(t, err)
	err = json.Unmarshal(stdout, &osmosisHostZone)
	require.NoError(t, err)

	// query juno host zone
	stdout, _, err = strideFullNode.ExecQuery(ctx,
		"stakeibc", "show-host-zone", junoCfg.ChainID,
	)
	require.NoError(t, err)
	err = json.Unmarshal(stdout, &junoHostZone)
	require.NoError(t, err)

	// Liquid stake some atom
	_, err = strideFullNode.ExecTx(ctx, strideUser.KeyName,
		"stakeibc", "liquid-stake",
		"1000000000000", gaiaCfg.Denom,
	)
	require.NoError(t, err)

	// Liquid stake some osmo
	_, err = strideFullNode.ExecTx(ctx, strideUser.KeyName,
		"stakeibc", "liquid-stake",
		"1000000000000", osmosisCfg.Denom,
	)
	require.NoError(t, err)

	// Liquid stake some juno
	_, err = strideFullNode.ExecTx(ctx, strideUser.KeyName,
		"stakeibc", "liquid-stake",
		"1000000000000", junoCfg.Denom,
	)
	require.NoError(t, err)

	err = test.WaitForBlocks(ctx, 100, stride, gaia, osmosis, juno)
	require.NoError(t, err)

}