package e2e

import (
	"fmt"
	"time"

	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	evidencetypes "github.com/cosmos/cosmos-sdk/x/evidence/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	ccv "github.com/cosmos/interchain-security/x/ccv/types"

	clienttypes "github.com/cosmos/ibc-go/v3/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v3/modules/core/04-channel/types"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
)

const (
	downtimeTestCase = iota
	doubleSignTestCase
)

// TestRelayAndApplySlashPacket tests that slash packets can be properly relayed
// from consumer to provider, handled by provider, with a VSC and jailing/tombstoning
// eventually effective on consumer and provider.
//
// Note: This method does not test the actual slash packet sending logic for downtime
// and double-signing, see TestValidatorDowntime and TestValidatorDoubleSigning for
// those types of tests.
func (s *CCVTestSuite) TestRelayAndApplySlashPacket() {

	testCases := []int{
		downtimeTestCase,
		doubleSignTestCase,
	}

	for _, tc := range testCases {

		// Reset test state
		s.SetupTest()

		// Setup CCV channel for all instantiated consumers
		s.SetupAllCCVChannels()

		validatorsPerChain := len(s.consumerChain.Vals.Validators)

		providerStakingKeeper := s.providerApp.GetE2eStakingKeeper()
		providerSlashingKeeper := s.providerApp.GetE2eSlashingKeeper()
		providerKeeper := s.providerApp.GetProviderKeeper()
		firstConsumerKeeper := s.getFirstBundle().GetKeeper()

		// get a cross-chain validator address, pubkey and balance
		tmVals := s.consumerChain.Vals.Validators
		tmVal := tmVals[0]

		val, err := tmVal.ToProto()
		s.Require().NoError(err)
		pubkey, err := cryptocodec.FromTmProtoPublicKey(val.GetPubKey())
		s.Require().Nil(err)
		consAddr := sdk.GetConsAddress(pubkey)
		valData, found := providerStakingKeeper.GetValidatorByConsAddr(s.providerCtx(), consAddr)
		s.Require().True(found)
		valOldBalance := valData.Tokens

		// create the validator's signing info record to allow jailing
		valInfo := slashingtypes.NewValidatorSigningInfo(consAddr, s.providerCtx().BlockHeight(),
			s.providerCtx().BlockHeight()-1, time.Time{}.UTC(), false, int64(0))
		providerSlashingKeeper.SetValidatorSigningInfo(s.providerCtx(), consAddr, valInfo)

		// get valseUpdateId for current block height on first consumer
		valsetUpdateId := firstConsumerKeeper.GetHeightValsetUpdateID(
			s.consumerCtx(), uint64(s.consumerCtx().BlockHeight()))

		// construct the slash packet with the validator address and power
		validator := abci.Validator{
			Address: tmVal.Address,
			Power:   tmVal.VotingPower,
		}

		// Construct packet data depending on the test case
		var infractionType stakingtypes.InfractionType

		if tc == downtimeTestCase {
			infractionType = stakingtypes.Downtime
		} else if tc == doubleSignTestCase {
			infractionType = stakingtypes.DoubleSign
		}
		packetData := ccv.NewSlashPacketData(validator, valsetUpdateId, infractionType).GetBytes()

		oldBlockTime := s.consumerCtx().BlockTime()
		timeout := uint64(oldBlockTime.Add(ccv.DefaultCCVTimeoutPeriod).UnixNano())
		packet := channeltypes.NewPacket(packetData, 1, ccv.ConsumerPortID, s.path.EndpointA.ChannelID,
			ccv.ProviderPortID, s.path.EndpointB.ChannelID, clienttypes.Height{}, timeout)

		// Send slash packet from the first consumer chain
		err = s.getFirstBundle().Path.EndpointA.SendPacket(packet)
		s.Require().NoError(err)

		if tc == downtimeTestCase {
			// Set outstanding slashing flag for first consumer if testing a downtime slash packet
			firstConsumerKeeper.SetOutstandingDowntime(s.consumerCtx(), consAddr)
		}

		// Note: RecvPacket advances two blocks. Let's say the provider is currently at height N.
		// The received slash packet will be handled during N, and the staking module will then
		// register a validator update from that packet during the endblocker of N. Then the ccv
		// module sends VSC packets during the endblocker of N. The new validator set will be
		// committed to in block N+1, and will be in effect for block N+2.

		valsetUpdateN := providerKeeper.GetValidatorSetUpdateId(s.providerCtx())

		// receive the downtime packet on the provider chain.
		// RecvPacket() calls the provider endblocker twice
		err = s.path.EndpointB.RecvPacket(packet)
		s.Require().NoError(err)

		// We've now advanced two blocks.

		// VSC packets should have been sent from provider during block N to each consumer
		expectedSentValsetUpdateId := valsetUpdateN
		for _, bundle := range s.consumerBundles {
			_, found = providerKeeper.GetVscSendTimestamp(s.providerCtx(),
				bundle.Chain.ChainID, expectedSentValsetUpdateId)
			s.Require().True(found)
		}

		// Confirm the valset update Id was incremented twice on provider,
		// since two endblockers have passed.
		valsetUpdateNPlus2 := providerKeeper.GetValidatorSetUpdateId(s.providerCtx())
		s.Require().Equal(valsetUpdateN+2, valsetUpdateNPlus2)

		// check that the validator was removed from the provider validator set
		s.Require().Len(s.providerChain.Vals.Validators, validatorsPerChain-1)

		for _, bundle := range s.consumerBundles {
			// Relay VSC packets from provider to each consumer
			relayAllCommittedPackets(s, s.providerChain, bundle.Path,
				ccv.ProviderPortID, bundle.Path.EndpointB.ChannelID, 1)

			// check that each consumer updated its VSC ID for the subsequent block
			consumerKeeper := bundle.GetKeeper()
			ctx := bundle.GetCtx()
			actualValsetUpdateID := consumerKeeper.GetHeightValsetUpdateID(
				ctx, uint64(ctx.BlockHeight())+1)
			s.Require().Equal(expectedSentValsetUpdateId, actualValsetUpdateID)

			// check that slashed validator was removed from each consumer validator set
			s.Require().Len(bundle.Chain.Vals.Validators, validatorsPerChain-1)
		}

		// check that the validator is successfully jailed on provider
		validatorJailed, ok := providerStakingKeeper.GetValidatorByConsAddr(s.providerCtx(), consAddr)
		s.Require().True(ok)
		s.Require().True(validatorJailed.Jailed)
		s.Require().Equal(validatorJailed.Status, stakingtypes.Unbonding)

		// check that the slashed validator's tokens were indeed slashed on provider
		var slashFraction sdk.Dec
		if tc == downtimeTestCase {
			slashFraction = providerSlashingKeeper.SlashFractionDowntime(s.providerCtx())

		} else if tc == doubleSignTestCase {
			slashFraction = providerSlashingKeeper.SlashFractionDoubleSign(s.providerCtx())
		}
		slashedAmount := slashFraction.Mul(valOldBalance.ToDec())

		resultingTokens := valOldBalance.Sub(slashedAmount.TruncateInt())
		s.Require().Equal(resultingTokens, validatorJailed.GetTokens())

		// check that the validator's unjailing time is updated on provider
		valSignInfo, found := providerSlashingKeeper.GetValidatorSigningInfo(s.providerCtx(), consAddr)
		s.Require().True(found)
		s.Require().True(valSignInfo.JailedUntil.After(s.providerCtx().BlockHeader().Time))

		if tc == downtimeTestCase {
			// check that the outstanding slashing flag is reset on first consumer,
			// since that consumer originally sent the slash packet
			pFlag := firstConsumerKeeper.OutstandingDowntime(s.consumerCtx(), consAddr)
			s.Require().False(pFlag)

			// check that slashing packet gets acknowledged successfully
			ack := channeltypes.NewResultAcknowledgement([]byte{byte(1)})
			err = s.path.EndpointA.AcknowledgePacket(packet, ack.Acknowledgement())
			s.Require().NoError(err)

		} else if tc == doubleSignTestCase {
			// check that validator was tombstoned on provider
			s.Require().True(valSignInfo.Tombstoned)
			s.Require().True(valSignInfo.JailedUntil.Equal(evidencetypes.DoubleSignJailEndTime))
		}
	}
}

func (s *CCVTestSuite) TestSlashPacketAcknowledgement() {
	providerKeeper := s.providerApp.GetProviderKeeper()
	consumerKeeper := s.consumerApp.GetConsumerKeeper()

	s.SetupCCVChannel(s.path)
	s.SetupTransferChannel()

	packet := channeltypes.NewPacket([]byte{}, 1, ccv.ConsumerPortID, s.path.EndpointA.ChannelID,
		ccv.ProviderPortID, s.path.EndpointB.ChannelID, clienttypes.Height{}, 0)

	ack := providerKeeper.OnRecvSlashPacket(s.providerCtx(), packet, ccv.SlashPacketData{})
	s.Require().NotNil(ack)

	err := consumerKeeper.OnAcknowledgementPacket(s.consumerCtx(), packet, channeltypes.NewResultAcknowledgement(ack.Acknowledgement()))
	s.Require().NoError(err)

	err = consumerKeeper.OnAcknowledgementPacket(s.consumerCtx(), packet, channeltypes.NewErrorAcknowledgement("another error"))
	s.Require().Error(err)
}

// TestHandleSlashPacketDoubleSigning tests the handling of a double-signing related slash packet, with e2e tests
func (suite *CCVTestSuite) TestHandleSlashPacketDoubleSigning() {
	providerKeeper := suite.providerApp.GetProviderKeeper()
	providerSlashingKeeper := suite.providerApp.GetE2eSlashingKeeper()
	providerStakingKeeper := suite.providerApp.GetE2eStakingKeeper()

	tmVal := suite.providerChain.Vals.Validators[0]
	consAddr := sdk.ConsAddress(tmVal.Address)

	// check that validator bonded status
	validator, found := providerStakingKeeper.GetValidatorByConsAddr(suite.providerCtx(), consAddr)
	suite.Require().True(found)
	suite.Require().Equal(stakingtypes.Bonded, validator.GetStatus())

	// set init VSC id for chain0
	providerKeeper.SetInitChainHeight(suite.providerCtx(), suite.consumerChain.ChainID, uint64(suite.providerCtx().BlockHeight()))

	// set validator signing-info
	providerSlashingKeeper.SetValidatorSigningInfo(
		suite.providerCtx(),
		consAddr,
		slashingtypes.ValidatorSigningInfo{Address: consAddr.String()},
	)

	_, err := providerKeeper.HandleSlashPacket(suite.providerCtx(), suite.consumerChain.ChainID,
		ccv.NewSlashPacketData(
			abci.Validator{Address: tmVal.Address, Power: 0},
			uint64(0),
			stakingtypes.DoubleSign,
		),
	)
	suite.NoError(err)

	// verify that validator is jailed in the staking and slashing modules' states
	suite.Require().True(providerStakingKeeper.IsValidatorJailed(suite.providerCtx(), consAddr))

	signingInfo, _ := providerSlashingKeeper.GetValidatorSigningInfo(suite.providerCtx(), consAddr)
	suite.Require().True(signingInfo.JailedUntil.Equal(evidencetypes.DoubleSignJailEndTime))
	suite.Require().True(signingInfo.Tombstoned)
}

// TestHandleSlashPacketErrors tests errors for the HandleSlashPacket method in an e2e testing setting
func (suite *CCVTestSuite) TestHandleSlashPacketErrors() {
	providerStakingKeeper := suite.providerApp.GetE2eStakingKeeper()
	ProviderKeeper := suite.providerApp.GetProviderKeeper()
	providerSlashingKeeper := suite.providerApp.GetE2eSlashingKeeper()
	consumerChainID := suite.consumerChain.ChainID

	// sync contexts block height
	ctx := suite.providerCtx()

	// expect an error if initial block height isn't set for consumer chain
	_, err := ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, ccv.SlashPacketData{})
	suite.Require().Error(err, "slash validator with invalid infraction height")

	// save VSC ID
	vID := ProviderKeeper.GetValidatorSetUpdateId(ctx)

	// remove block height for current VSC ID
	ProviderKeeper.DeleteValsetUpdateBlockHeight(ctx, vID)

	// expect an error if block height mapping VSC ID is zero
	_, err = ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, ccv.SlashPacketData{ValsetUpdateId: vID})
	suite.Require().Error(err, "slash with height mapping to zero")

	// construct slashing packet with non existing validator
	slashingPkt := ccv.NewSlashPacketData(
		abci.Validator{Address: ed25519.GenPrivKey().PubKey().Address(),
			Power: int64(0)}, uint64(0), stakingtypes.Downtime,
	)

	// Set initial block height for consumer chain
	ProviderKeeper.SetInitChainHeight(ctx, consumerChainID, uint64(ctx.BlockHeight()))

	// expect the slash to not succeed if validator doesn't exist
	success, err := ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, slashingPkt)
	suite.Require().NoError(err, "slashing an unknown validator should not result in error")
	suite.Require().False(success, "did slash unknown validator")

	// jail an existing validator
	val := suite.providerChain.Vals.Validators[0]
	consAddr := sdk.ConsAddress(val.Address)
	providerStakingKeeper.Jail(ctx, consAddr)
	// commit block to set VSC ID
	suite.coordinator.CommitBlock(suite.providerChain)
	// Update suite.ctx bc CommitBlock updates only providerChain's current header block height
	ctx = suite.providerChain.GetContext()
	suite.Require().NotZero(ProviderKeeper.GetValsetUpdateBlockHeight(ctx, vID))

	// create validator signing info
	valInfo := slashingtypes.NewValidatorSigningInfo(sdk.ConsAddress(val.Address), ctx.BlockHeight(),
		ctx.BlockHeight()-1, time.Time{}.UTC(), false, int64(0))
	providerSlashingKeeper.SetValidatorSigningInfo(ctx, sdk.ConsAddress(val.Address), valInfo)

	// update validator address and VSC ID
	slashingPkt.Validator.Address = val.Address
	slashingPkt.ValsetUpdateId = vID

	// expect to slash and jail validator
	_, err = ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, slashingPkt)
	suite.Require().NoError(err, "did slash jail validator")

	// expect error when infraction type in unspecified
	tmAddr := suite.providerChain.Vals.Validators[1].Address
	slashingPkt.Validator.Address = tmAddr
	slashingPkt.Infraction = stakingtypes.InfractionEmpty

	valInfo.Address = sdk.ConsAddress(tmAddr).String()
	providerSlashingKeeper.SetValidatorSigningInfo(ctx, sdk.ConsAddress(tmAddr), valInfo)

	_, err = ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, slashingPkt)
	suite.Require().EqualError(err, fmt.Sprintf("invalid infraction type: %v", stakingtypes.InfractionEmpty))

	// expect to slash jail validator
	slashingPkt.Infraction = stakingtypes.DoubleSign
	_, err = ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, slashingPkt)
	suite.Require().NoError(err)

	// expect the slash to not succeed when validator is tombstoned
	success, _ = ProviderKeeper.HandleSlashPacket(ctx, consumerChainID, slashingPkt)
	suite.Require().False(success)
}

// TestHandleSlashPacketDistribution tests the slashing of an undelegation balance
// by varying the slash packet VSC ID mapping to infraction heights
// lesser, equal or greater than the undelegation entry creation height
func (suite *CCVTestSuite) TestHandleSlashPacketDistribution() {
	providerKeeper := suite.providerApp.GetProviderKeeper()
	providerStakingKeeper := suite.providerApp.GetE2eStakingKeeper()
	providerSlashingKeeper := suite.providerApp.GetE2eSlashingKeeper()

	// choose a validator
	tmValidator := suite.providerChain.Vals.Validators[0]
	valAddr, err := sdk.ValAddressFromHex(tmValidator.Address.String())
	suite.Require().NoError(err)

	validator, found := providerStakingKeeper.GetValidator(suite.providerChain.GetContext(), valAddr)
	suite.Require().True(found)

	// unbonding operations parameters
	delAddr := suite.providerChain.SenderAccount.GetAddress()
	bondAmt := sdk.NewInt(1000000)

	// new delegator shares used
	testShares := sdk.Dec{}

	// setup the test with a delegation, a no-op and an undelegation
	setupOperations := []struct {
		fn func(suite *CCVTestSuite) error
	}{
		{
			func(suite *CCVTestSuite) error {
				testShares, err = providerStakingKeeper.Delegate(suite.providerChain.GetContext(), delAddr, bondAmt, stakingtypes.Unbonded, stakingtypes.Validator(validator), true)
				return err
			},
		}, {
			func(suite *CCVTestSuite) error {
				return nil
			},
		}, {
			// undelegate a quarter of the new shares created
			func(suite *CCVTestSuite) error {
				_, err = providerStakingKeeper.Undelegate(suite.providerChain.GetContext(), delAddr, valAddr, testShares.QuoInt64(4))
				return err
			},
		},
	}

	// execute the setup operations, distributed uniformly in three blocks.
	// For each of them, save their current VSC Id value which map correspond respectively
	// to the block heights lesser, equal and greater than the undelegation creation height.
	vscIDs := make([]uint64, 0, 3)
	for _, so := range setupOperations {
		err := so.fn(suite)
		suite.Require().NoError(err)

		vscIDs = append(vscIDs, providerKeeper.GetValidatorSetUpdateId(suite.providerChain.GetContext()))
		suite.providerChain.NextBlock()
	}

	// create validator signing info to test slashing
	providerSlashingKeeper.SetValidatorSigningInfo(
		suite.providerChain.GetContext(),
		sdk.ConsAddress(tmValidator.Address),
		slashingtypes.ValidatorSigningInfo{Address: tmValidator.Address.String()},
	)

	// the test cases verify that only the unbonding tokens get slashed for the VSC ids
	// mapping to the block heights before and during the undelegation otherwise not.
	testCases := []struct {
		expSlash bool
		vscID    uint64
	}{
		{expSlash: true, vscID: vscIDs[0]},
		{expSlash: true, vscID: vscIDs[1]},
		{expSlash: false, vscID: vscIDs[2]},
	}

	// save unbonding balance before slashing tests
	ubd, found := providerStakingKeeper.GetUnbondingDelegation(
		suite.providerChain.GetContext(), delAddr, valAddr)
	suite.Require().True(found)
	ubdBalance := ubd.Entries[0].Balance

	for _, tc := range testCases {
		slashPacket := ccv.NewSlashPacketData(
			abci.Validator{Address: tmValidator.Address, Power: tmValidator.VotingPower},
			tc.vscID,
			stakingtypes.Downtime,
		)

		// slash
		_, err := providerKeeper.HandleSlashPacket(suite.providerChain.GetContext(), suite.consumerChain.ChainID, slashPacket)
		suite.Require().NoError(err)

		ubd, found := providerStakingKeeper.GetUnbondingDelegation(suite.providerChain.GetContext(), delAddr, valAddr)
		suite.Require().True(found)

		isUbdSlashed := ubdBalance.GT(ubd.Entries[0].Balance)
		suite.Require().True(tc.expSlash == isUbdSlashed)

		// update balance
		ubdBalance = ubd.Entries[0].Balance
	}
}

// TestValidatorDowntime tests if a slash packet is sent
// and if the outstanding slashing flag is switched
// when a validator has downtime on the slashing module
func (suite *CCVTestSuite) TestValidatorDowntime() {
	// initial setup
	suite.SetupCCVChannel(suite.path)
	suite.SendEmptyVSCPacket()

	consumerKeeper := suite.consumerApp.GetConsumerKeeper()
	consumerSlashingKeeper := suite.consumerApp.GetE2eSlashingKeeper()
	consumerIBCKeeper := suite.consumerApp.GetIBCKeeper()

	// sync suite context after CCV channel is established
	ctx := suite.consumerCtx()

	channelID := suite.path.EndpointA.ChannelID

	// pick a cross-chain validator
	vals := consumerKeeper.GetAllCCValidator(ctx)
	consAddr := sdk.ConsAddress(vals[0].Address)

	// save next sequence before sending a slash packet
	seq, ok := consumerIBCKeeper.ChannelKeeper.GetNextSequenceSend(
		ctx, ccv.ConsumerPortID, channelID)
	suite.Require().True(ok)

	// Sign 100 blocks
	valPower := int64(1)
	height, signedBlocksWindow := int64(0), consumerSlashingKeeper.SignedBlocksWindow(ctx)
	for ; height < signedBlocksWindow; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, true)
	}

	missedBlockThreshold := (2 * signedBlocksWindow) - consumerSlashingKeeper.MinSignedPerWindow(ctx)
	ctx = suite.consumerCtx()

	// construct slash packet to be sent and get its commit
	packetData := ccv.NewSlashPacketData(
		abci.Validator{Address: vals[0].Address, Power: valPower},
		// get the VSC ID mapping the infraction height
		consumerKeeper.GetHeightValsetUpdateID(ctx, uint64(missedBlockThreshold-sdk.ValidatorUpdateDelay-1)),
		stakingtypes.Downtime,
	)
	expCommit := suite.commitSlashPacket(ctx, packetData)

	// Miss 50 blocks and expect a slash packet to be sent
	for ; height <= missedBlockThreshold; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, false)
	}

	ctx = suite.consumerCtx()

	// check validator signing info
	res, _ := consumerSlashingKeeper.GetValidatorSigningInfo(ctx, consAddr)
	// expect increased jail time
	suite.Require().True(res.JailedUntil.Equal(ctx.BlockTime().Add(consumerSlashingKeeper.DowntimeJailDuration(ctx))), "did not update validator jailed until signing info")
	// expect missed block counters reseted
	suite.Require().Zero(res.MissedBlocksCounter, "did not reset validator missed block counter")
	suite.Require().Zero(res.IndexOffset)
	consumerSlashingKeeper.IterateValidatorMissedBlockBitArray(ctx, consAddr, func(_ int64, missed bool) bool {
		suite.Require().True(missed)
		return false
	})

	// check that slash packet is queued
	pendingPackets := consumerKeeper.GetPendingPackets(ctx)
	suite.Require().NotEmpty(pendingPackets.List, "pending packets empty")
	suite.Require().Len(pendingPackets.List, 1, "pending packets len should be 1 is %d", len(pendingPackets.List))

	// clear queue, commit packets
	suite.consumerApp.GetConsumerKeeper().SendPackets(ctx)

	// check queue was cleared
	pendingPackets = suite.consumerApp.GetConsumerKeeper().GetPendingPackets(ctx)
	suite.Require().Empty(pendingPackets.List, "pending packets NOT empty")

	// verify that the slash packet was sent
	gotCommit := consumerIBCKeeper.ChannelKeeper.GetPacketCommitment(ctx, ccv.ConsumerPortID, channelID, seq)
	suite.Require().NotNil(gotCommit, "did not found slash packet commitment")
	suite.Require().EqualValues(expCommit, gotCommit, "invalid slash packet commitment")

	// verify that the slash packet was sent
	suite.Require().True(consumerKeeper.OutstandingDowntime(ctx, consAddr))

	// check that the outstanding slashing flag prevents the jailed validator to keep missing block
	for ; height < missedBlockThreshold+signedBlocksWindow; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, false)
	}

	res, _ = consumerSlashingKeeper.GetValidatorSigningInfo(ctx, consAddr)

	suite.Require().Zero(res.MissedBlocksCounter, "did not reset validator missed block counter")
	suite.Require().Zero(res.IndexOffset)
	consumerSlashingKeeper.IterateValidatorMissedBlockBitArray(ctx, consAddr, func(_ int64, missed bool) bool {
		suite.Require().True(missed, "did not reset validator missed block bit array")
		return false
	})
}

// TestValidatorDoubleSigning tests if a slash packet is sent
// when a double-signing evidence is handled by the evidence module
func (suite *CCVTestSuite) TestValidatorDoubleSigning() {
	// initial setup
	suite.SetupCCVChannel(suite.path)
	suite.SendEmptyVSCPacket()

	// sync suite context after CCV channel is established
	ctx := suite.consumerCtx()

	channelID := suite.path.EndpointA.ChannelID

	// create a validator pubkey and address
	// note that the validator wont't necessarily be in valset to due the TM delay
	pubkey := ed25519.GenPrivKey().PubKey()
	consAddr := sdk.ConsAddress(pubkey.Address())

	// set an arbitrary infraction height
	infractionHeight := ctx.BlockHeight() - 1
	power := int64(100)

	// create evidence
	e := &evidencetypes.Equivocation{
		Height:           infractionHeight,
		Power:            power,
		Time:             time.Now().UTC(),
		ConsensusAddress: consAddr.String(),
	}

	// add validator signing-info to the store
	suite.consumerApp.GetE2eSlashingKeeper().SetValidatorSigningInfo(ctx, consAddr, slashingtypes.ValidatorSigningInfo{
		Address:    consAddr.String(),
		Tombstoned: false,
	})

	// save next sequence before sending a slash packet
	seq, ok := suite.consumerApp.GetIBCKeeper().ChannelKeeper.GetNextSequenceSend(ctx, ccv.ConsumerPortID, channelID)
	suite.Require().True(ok)

	// construct slash packet data and get the expcted commit hash
	packetData := ccv.NewSlashPacketData(
		abci.Validator{Address: consAddr.Bytes(), Power: power},
		// get VSC ID mapping to the infraction height with the TM delay substracted
		suite.consumerApp.GetConsumerKeeper().GetHeightValsetUpdateID(ctx, uint64(infractionHeight-sdk.ValidatorUpdateDelay)),
		stakingtypes.DoubleSign,
	)
	expCommit := suite.commitSlashPacket(ctx, packetData)

	// expect to send slash packet when handling double-sign evidence
	suite.consumerApp.GetE2eEvidenceKeeper().HandleEquivocationEvidence(ctx, e)

	// check slash packet is queued
	pendingPackets := suite.consumerApp.GetConsumerKeeper().GetPendingPackets(ctx)
	suite.Require().NotEmpty(pendingPackets.List, "pending packets empty")
	suite.Require().Len(pendingPackets.List, 1, "pending packets len should be 1 is %d", len(pendingPackets.List))

	// clear queue, commit packets
	suite.consumerApp.GetConsumerKeeper().SendPackets(ctx)

	// check queue was cleared
	pendingPackets = suite.consumerApp.GetConsumerKeeper().GetPendingPackets(ctx)
	suite.Require().Empty(pendingPackets.List, "pending packets NOT empty")

	// check slash packet is sent
	gotCommit := suite.consumerApp.GetIBCKeeper().ChannelKeeper.GetPacketCommitment(ctx, ccv.ConsumerPortID, channelID, seq)
	suite.NotNil(gotCommit)

	suite.Require().EqualValues(expCommit, gotCommit)
}

// TestQueueAndSendSlashPacket tests the integration of QueueSlashPacket with SendPackets.
// In normal operation slash packets are queued in BeginBlock and sent in EndBlock.
func (suite *CCVTestSuite) TestQueueAndSendSlashPacket() {
	suite.SetupCCVChannel(suite.path)

	consumerKeeper := suite.consumerApp.GetConsumerKeeper()
	consumerIBCKeeper := suite.consumerApp.GetIBCKeeper()

	ctx := suite.consumerChain.GetContext()
	channelID := suite.path.EndpointA.ChannelID

	// check that CCV channel isn't established
	_, ok := consumerKeeper.GetProviderChannel(ctx)
	suite.Require().False(ok)

	// expect to store 4 slash requests for downtime
	// and 4 slash request for double-signing
	type slashedVal struct {
		validator  abci.Validator
		infraction stakingtypes.InfractionType
	}
	slashedVals := []slashedVal{}

	infraction := stakingtypes.Downtime
	for j := 0; j < 2; j++ {
		for i := 0; i < 4; i++ {
			addr := ed25519.GenPrivKey().PubKey().Address()
			val := abci.Validator{
				Address: addr}
			consumerKeeper.QueueSlashPacket(ctx, val, 0, infraction)
			slashedVals = append(slashedVals, slashedVal{validator: val, infraction: infraction})
		}
		infraction = stakingtypes.DoubleSign
	}

	// expect to store a duplicate for each slash request
	// in order to test the outstanding downtime logic
	for _, sv := range slashedVals {
		consumerKeeper.QueueSlashPacket(ctx, sv.validator, 0, sv.infraction)
	}

	// verify that all requests are stored except for
	// the downtime slash request duplicates
	dataPackets := consumerKeeper.GetPendingPackets(ctx)
	suite.Require().NotEmpty(dataPackets)
	suite.Require().Len(dataPackets.GetList(), 12)

	// save consumer next sequence
	seq, _ := consumerIBCKeeper.ChannelKeeper.GetNextSequenceSend(ctx, ccv.ConsumerPortID, channelID)

	// establish ccv channel by sending an empty VSC packet to consumer endpoint
	suite.SendEmptyVSCPacket()

	// check that each pending data packet is sent once
	for i := 0; i < 12; i++ {
		commit := consumerIBCKeeper.ChannelKeeper.GetPacketCommitment(ctx, ccv.ConsumerPortID, channelID, seq+uint64(i))
		suite.Require().NotNil(commit)
	}

	// check that outstanding downtime flags
	// are all set to true for validators slashed for downtime requests
	for i := 0; i < 4; i++ {
		consAddr := sdk.ConsAddress(slashedVals[i].validator.Address)
		suite.Require().True(consumerKeeper.OutstandingDowntime(ctx, consAddr))
	}

	// send all pending packets - only slash packets should be queued in this test
	consumerKeeper.SendPackets(ctx)

	// check that pending data packets got cleared
	dataPackets = consumerKeeper.GetPendingPackets(ctx)
	suite.Require().Empty(dataPackets)
	suite.Require().Len(dataPackets.GetList(), 0)
}
