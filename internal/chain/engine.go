package chain

import (
	"bytes"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	lru "github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"

	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/consensus/engine"
	"github.com/harmony-one/harmony/consensus/quorum"
	"github.com/harmony-one/harmony/consensus/reward"
	"github.com/harmony-one/harmony/consensus/signature"
	"github.com/harmony-one/harmony/core/state"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/bls"
	bls_cosi "github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/shard/committee"
	"github.com/harmony-one/harmony/staking/availability"
	"github.com/harmony-one/harmony/staking/slash"
	staking "github.com/harmony-one/harmony/staking/types"
)

const (
	verifiedSigCache = 20
	epochCtxCache    = 20
)

type engineImpl struct {
	beacon engine.ChainReader

	// Caching field
	epochCtxCache    *lru.Cache // epochCtxKey -> epochCtx
	verifiedSigCache *lru.Cache // verifiedSigKey -> struct{}{}
}

// NewEngine creates Engine with some cache
func NewEngine() *engineImpl {
	sigCache, _ := lru.New(verifiedSigCache)
	epochCtxCache, _ := lru.New(epochCtxCache)
	return &engineImpl{
		beacon:           nil,
		epochCtxCache:    epochCtxCache,
		verifiedSigCache: sigCache,
	}
}

func (e *engineImpl) Beaconchain() engine.ChainReader {
	return e.beacon
}

// SetBeaconchain assigns the beaconchain handle used
func (e *engineImpl) SetBeaconchain(beaconchain engine.ChainReader) {
	e.beacon = beaconchain
}

// VerifyHeader checks whether a header conforms to the consensus rules of the bft engine.
// Note that each block header contains the bls signature of the parent block
func (e *engineImpl) VerifyHeader(chain engine.ChainReader, header *block.Header, seal bool) error {
	parentHeader := chain.GetHeader(header.ParentHash(), header.Number().Uint64()-1)
	if parentHeader == nil {
		return engine.ErrUnknownAncestor
	}
	if seal {
		if err := e.VerifySeal(chain, header); err != nil {
			return err
		}
	}
	return nil
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
// WARN: Do not use VerifyHeaders for now. Currently a header verification can only
// success when the previous header is written to block chain
// TODO: Revisit and correct this function when adding epochChain
func (e *engineImpl) VerifyHeaders(chain engine.ChainReader, headers []*block.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort, results := make(chan struct{}), make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := e.VerifyHeader(chain, header, seals[i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()

	return abort, results
}

// VerifyShardState implements Engine, checking the shardstate is valid at epoch transition
func (e *engineImpl) VerifyShardState(
	bc engine.ChainReader, beacon engine.ChainReader, header *block.Header,
) error {
	if bc.ShardID() != header.ShardID() {
		return errors.Errorf(
			"[VerifyShardState] shardID not match %d %d", bc.ShardID(), header.ShardID(),
		)
	}
	headerShardStateBytes := header.ShardState()
	// TODO: figure out leader withhold shardState
	if len(headerShardStateBytes) == 0 {
		return nil
	}
	shardState, err := bc.SuperCommitteeForNextEpoch(beacon, header, true)
	if err != nil {
		return err
	}

	isStaking := false
	if shardState.Epoch != nil && bc.Config().IsStaking(shardState.Epoch) {
		isStaking = true
	}
	shardStateBytes, err := shard.EncodeWrapper(*shardState, isStaking)
	if err != nil {
		return errors.Wrapf(
			err, "[VerifyShardState] ShardState Encoding had error",
		)
	}

	if !bytes.Equal(shardStateBytes, headerShardStateBytes) {
		return errors.New("shard state header did not match as expected")
	}

	return nil
}

// VerifySeal implements Engine, checking whether the given block's parent block satisfies
// the PoS difficulty requirements, i.e. >= 2f+1 valid signatures from the committee
// Note that each block header contains the bls signature of the parent block
func (e *engineImpl) VerifySeal(chain engine.ChainReader, header *block.Header) error {
	if chain.CurrentHeader().Number().Uint64() <= uint64(1) {
		return nil
	}
	if header == nil {
		return errors.New("[VerifySeal] nil block header")
	}

	parentHash := header.ParentHash()
	parentHeader := chain.GetHeader(parentHash, header.Number().Uint64()-1)
	if parentHeader == nil {
		return errors.New("[VerifySeal] no parent header found")
	}

	sig := header.LastCommitSignature()
	bitmap := header.LastCommitBitmap()

	if err := e.verifyHeaderSignatureCached(chain, parentHeader, sig, bitmap); err != nil {
		return errors.Wrapf(err, "verify signature for parent %s", parentHash.String())
	}
	return nil
}

// Finalize implements Engine, accumulating the block rewards,
// setting the final state and assembling the block.
// sigsReady signal indicates whether the commit sigs are populated in the header object.
func (e *engineImpl) Finalize(
	chain engine.ChainReader, header *block.Header,
	state *state.DB, txs []*types.Transaction,
	receipts []*types.Receipt, outcxs []*types.CXReceipt,
	incxs []*types.CXReceiptsProof, stks staking.StakingTransactions,
	doubleSigners slash.Records, sigsReady chan bool, viewID func() uint64,
) (*types.Block, reward.Reader, error) {

	isBeaconChain := header.ShardID() == shard.BeaconChainShardID
	inStakingEra := chain.Config().IsStaking(header.Epoch())

	// Process Undelegations, set LastEpochInCommittee and set EPoS status
	// Needs to be before AccumulateRewardsAndCountSigs
	if IsCommitteeSelectionBlock(chain, header) {
		if err := payoutUndelegations(chain, header, state); err != nil {
			return nil, nil, err
		}

		// Needs to be after payoutUndelegations because payoutUndelegations
		// depends on the old LastEpochInCommittee
		if err := setLastEpochInCommittee(header, state); err != nil {
			return nil, nil, err
		}

		curShardState, err := chain.ReadShardState(chain.CurrentBlock().Epoch())
		if err != nil {
			return nil, nil, err
		}
		// Needs to be before AccumulateRewardsAndCountSigs because
		// ComputeAndMutateEPOSStatus depends on the signing counts that's
		// consistent with the counts when the new shardState was proposed.
		// Refer to committee.IsEligibleForEPoSAuction()
		for _, addr := range curShardState.StakedValidators().Addrs {
			if err := availability.ComputeAndMutateEPOSStatus(
				chain, state, addr,
			); err != nil {
				return nil, nil, err
			}
		}
	}

	// Accumulate block rewards and commit the final state root
	// Header seems complete, assemble into a block and return
	payout, err := AccumulateRewardsAndCountSigs(
		chain, state, header, e.Beaconchain(), sigsReady,
	)
	if err != nil {
		return nil, nil, err
	}

	// Apply slashes
	if isBeaconChain && inStakingEra && len(doubleSigners) > 0 {
		if err := applySlashes(chain, header, state, doubleSigners); err != nil {
			return nil, nil, err
		}
	} else if len(doubleSigners) > 0 {
		return nil, nil, errors.New("slashes proposed in non-beacon chain or non-staking epoch")
	}

	// ViewID setting needs to happen after commig sig reward logic for pipelining reason.
	// TODO: make the viewID fetch from caller of the block proposal.
	header.SetViewID(new(big.Int).SetUint64(viewID()))

	// Finalize the state root
	header.SetRoot(state.IntermediateRoot(chain.Config().IsS3(header.Epoch())))
	return types.NewBlock(header, txs, receipts, outcxs, incxs, stks), payout, nil
}

// Withdraw unlocked tokens to the delegators' accounts
func payoutUndelegations(
	chain engine.ChainReader, header *block.Header, state *state.DB,
) error {
	currentHeader := chain.CurrentHeader()
	nowEpoch, blockNow := currentHeader.Epoch(), currentHeader.Number()
	utils.AnalysisStart("payoutUndelegations", nowEpoch, blockNow)
	defer utils.AnalysisEnd("payoutUndelegations", nowEpoch, blockNow)

	validators, err := chain.ReadValidatorList()
	countTrack := map[common.Address]int{}
	if err != nil {
		const msg = "[Finalize] failed to read all validators"
		return errors.New(msg)
	}
	// Payout undelegated/unlocked tokens
	lockPeriod := GetLockPeriodInEpoch(chain, header.Epoch())
	noEarlyUnlock := chain.Config().IsNoEarlyUnlock(header.Epoch())
	for _, validator := range validators {
		wrapper, err := state.ValidatorWrapper(validator)
		if err != nil {
			return errors.New(
				"[Finalize] failed to get validator from state to finalize",
			)
		}
		for i := range wrapper.Delegations {
			delegation := &wrapper.Delegations[i]
			totalWithdraw := delegation.RemoveUnlockedUndelegations(
				header.Epoch(), wrapper.LastEpochInCommittee, lockPeriod, noEarlyUnlock,
			)
			if totalWithdraw.Sign() != 0 {
				state.AddBalance(delegation.DelegatorAddress, totalWithdraw)
			}
		}
		countTrack[validator] = len(wrapper.Delegations)
	}

	utils.Logger().Debug().
		Uint64("epoch", header.Epoch().Uint64()).
		Uint64("block-number", header.Number().Uint64()).
		Interface("count-track", countTrack).
		Msg("paid out delegations")

	return nil
}

// IsCommitteeSelectionBlock checks if the given header is for the committee selection block
// which can only occur on beacon chain and if epoch > pre-staking epoch.
func IsCommitteeSelectionBlock(chain engine.ChainReader, header *block.Header) bool {
	isBeaconChain := header.ShardID() == shard.BeaconChainShardID
	inPreStakingEra := chain.Config().IsPreStaking(header.Epoch())
	return isBeaconChain && header.IsLastBlockInEpoch() && inPreStakingEra
}

func setLastEpochInCommittee(header *block.Header, state *state.DB) error {
	newShardState, err := header.GetShardState()
	if err != nil {
		const msg = "[Finalize] failed to read shard state"
		return errors.New(msg)
	}
	for _, addr := range newShardState.StakedValidators().Addrs {
		wrapper, err := state.ValidatorWrapper(addr)
		if err != nil {
			return errors.New(
				"[Finalize] failed to get validator from state to finalize",
			)
		}
		wrapper.LastEpochInCommittee = newShardState.Epoch
	}
	return nil
}

func applySlashes(
	chain engine.ChainReader,
	header *block.Header,
	state *state.DB,
	doubleSigners slash.Records,
) error {
	type keyStruct struct {
		height  uint64
		viewID  uint64
		shardID uint32
		epoch   uint64
	}

	groupedRecords := map[keyStruct]slash.Records{}

	// First group slashes by same signed blocks
	for i := range doubleSigners {
		thisKey := keyStruct{
			height:  doubleSigners[i].Evidence.Height,
			viewID:  doubleSigners[i].Evidence.ViewID,
			shardID: doubleSigners[i].Evidence.Moment.ShardID,
			epoch:   doubleSigners[i].Evidence.Moment.Epoch.Uint64(),
		}
		groupedRecords[thisKey] = append(groupedRecords[thisKey], doubleSigners[i])
	}

	sortedKeys := []keyStruct{}

	for key := range groupedRecords {
		sortedKeys = append(sortedKeys, key)
	}

	// Sort them so the slashes are always consistent
	sort.SliceStable(sortedKeys, func(i, j int) bool {
		if sortedKeys[i].shardID < sortedKeys[j].shardID {
			return true
		} else if sortedKeys[i].height < sortedKeys[j].height {
			return true
		} else if sortedKeys[i].viewID < sortedKeys[j].viewID {
			return true
		}
		return false
	})

	// Do the slashing by groups in the sorted order
	for _, key := range sortedKeys {
		records := groupedRecords[key]
		superCommittee, err := chain.ReadShardState(big.NewInt(int64(key.epoch)))

		if err != nil {
			return errors.New("could not read shard state")
		}

		subComm, err := superCommittee.FindCommitteeByID(key.shardID)

		if err != nil {
			return errors.New("could not find shard committee")
		}

		// Apply the slashes, invariant: assume been verified as legit slash by this point
		var slashApplied *slash.Application
		votingPower, err := lookupVotingPower(
			big.NewInt(int64(key.epoch)), subComm,
		)
		if err != nil {
			return errors.Wrapf(err, "could not lookup cached voting power in slash application")
		}
		rate := slash.Rate(votingPower, records)
		utils.Logger().Info().
			Str("rate", rate.String()).
			RawJSON("records", []byte(records.String())).
			Msg("now applying slash to state during block finalization")
		if slashApplied, err = slash.Apply(
			chain,
			state,
			records,
			rate,
		); err != nil {
			return errors.New("[Finalize] could not apply slash")
		}

		utils.Logger().Info().
			Str("rate", rate.String()).
			RawJSON("records", []byte(records.String())).
			RawJSON("applied", []byte(slashApplied.String())).
			Msg("slash applied successfully")
	}
	return nil
}

// VerifyHeaderSignature verifies the signature of the given header.
// Similiar to VerifyHeader, which is only for verifying the block headers of one's own chain, this verification
// is used for verifying "incoming" block header against commit signature and bitmap sent from the other chain cross-shard via libp2p.
// i.e. this header verification api is more flexible since the caller specifies which commit signature and bitmap to use
// for verifying the block header, which is necessary for cross-shard block header verification. Example of such is cross-shard transaction.
func (e *engineImpl) VerifyHeaderSignature(chain engine.ChainReader, header *block.Header, commitSig bls_cosi.SerializedSignature, commitBitmap []byte) error {
	if chain.CurrentHeader().Number().Uint64() <= uint64(1) {
		return nil
	}
	return e.verifyHeaderSignatureCached(chain, header, commitSig, commitBitmap)
}

func (e *engineImpl) verifyHeaderSignatureCached(chain engine.ChainReader, header *block.Header, commitSig bls_cosi.SerializedSignature, commitBitmap []byte) error {
	key := newVerifiedSigKey(header.Hash(), commitSig, commitBitmap)
	if _, ok := e.verifiedSigCache.Get(key); ok {
		return nil
	}
	if err := e.verifyHeaderSignature(chain, header, commitSig, commitBitmap); err != nil {
		return err
	}
	e.verifiedSigCache.Add(key, struct{}{})
	return nil
}

func (e *engineImpl) verifyHeaderSignature(chain engine.ChainReader, header *block.Header, commitSig bls_cosi.SerializedSignature, commitBitmap []byte) error {
	ec, ok := e.getCachedEpochCtx(header)
	if !ok {
		// Epoch context not in cache, read from chain
		var err error
		ec, err = readEpochCtxFromChain(chain, header.Epoch(), header.ShardID())
		if err != nil {
			return err
		}
	}

	var (
		pubKeys    = ec.pubKeys
		qrVerifier = ec.qrVerifier
	)
	aggSig, mask, err := DecodeSigBitmap(commitSig, commitBitmap, pubKeys)
	if err != nil {
		return errors.Wrap(err, "deserialize signature and bitmap")
	}
	// Verify signature, mask against quorum.Verifier and publicKeys
	if !qrVerifier.IsQuorumAchievedByMask(mask) {
		return errors.New("not enough signature collected")
	}
	commitPayload := signature.ConstructCommitPayload(chain,
		header.Epoch(), header.Hash(), header.Number().Uint64(), header.ViewID().Uint64())

	if !aggSig.VerifyHash(mask.AggregatePublic, commitPayload) {
		return errors.New("Unable to verify aggregated signature for block")
	}
	return nil
}

func (e *engineImpl) getCachedEpochCtx(header *block.Header) (*epochCtx, bool) {
	ecKey := newEpochCtxKeyFromHeader(header)
	ec, ok := e.epochCtxCache.Get(ecKey)
	if !ok || ec == nil {
		return nil, false
	}
	return ec.(*epochCtx), true
}

// Support 512 at most validator nodes
const bitmapKeyBytes = 64

// verifiedSigKey is the key for caching header verification results
type verifiedSigKey struct {
	blockHash common.Hash
	signature bls_cosi.SerializedSignature
	bitmap    [bitmapKeyBytes]byte
}

func newVerifiedSigKey(blockHash common.Hash, sig bls_cosi.SerializedSignature, bitmap []byte) verifiedSigKey {
	var keyBM [bitmapKeyBytes]byte
	copy(keyBM[:], bitmap)

	return verifiedSigKey{
		blockHash: blockHash,
		signature: sig,
		bitmap:    keyBM,
	}
}

type (
	// epochCtxKey is the key for caching epochCtx
	epochCtxKey struct {
		shardID uint32
		epoch   uint64
	}

	// epochCtx is the epoch's context used for signature verification.
	// The value is fixed for each epoch and is cached in engineImpl.
	epochCtx struct {
		qrVerifier quorum.Verifier
		pubKeys    []bls.PublicKeyWrapper
	}
)

func newEpochCtxKeyFromHeader(header *block.Header) epochCtxKey {
	return epochCtxKey{
		shardID: header.ShardID(),
		epoch:   header.Epoch().Uint64(),
	}
}

func readEpochCtxFromChain(chain engine.ChainReader, epoch *big.Int, targetShardID uint32) (*epochCtx, error) {
	ss, err := readShardState(chain, epoch, targetShardID)
	if err != nil {
		return nil, err
	}
	shardComm, err := ss.FindCommitteeByID(targetShardID)
	if err != nil {
		return nil, err
	}
	pubKeys, err := shardComm.BLSPublicKeys()
	if err != nil {
		return nil, err
	}
	isStaking := chain.Config().IsStaking(epoch)
	qrVerifier, err := quorum.NewVerifier(shardComm, epoch, isStaking)
	if err != nil {
		return nil, err
	}
	return &epochCtx{
		qrVerifier: qrVerifier,
		pubKeys:    pubKeys,
	}, nil
}

func readShardState(chain engine.ChainReader, epoch *big.Int, targetShardID uint32) (*shard.State, error) {
	// When doing cross shard, we need recalcualte the shard state since we don't have
	// shard state of other shards
	if needRecalculateStateShard(chain, epoch, targetShardID) {
		shardState, err := committee.WithStakingEnabled.Compute(epoch, chain)
		if err != nil {
			return nil, errors.Wrapf(err, "compute shard state for epoch %v", epoch)
		}
		return shardState, nil

	} else {
		shardState, err := chain.ReadShardState(epoch)
		if err != nil {
			return nil, errors.Wrapf(err, "read shard state for epoch %v", epoch)
		}
		return shardState, nil
	}
}

// only recalculate for non-staking epoch and targetShardID is not the same
// as engine
func needRecalculateStateShard(chain engine.ChainReader, epoch *big.Int, targetShardID uint32) bool {
	if chain.Config().IsStaking(epoch) {
		return false
	}
	return targetShardID != chain.ShardID()
}

// GetLockPeriodInEpoch returns the delegation lock period for the given chain
func GetLockPeriodInEpoch(chain engine.ChainReader, epoch *big.Int) int {
	lockPeriod := staking.LockPeriodInEpoch
	if chain.Config().IsRedelegation(epoch) {
		lockPeriod = staking.LockPeriodInEpoch
	} else if chain.Config().IsQuickUnlock(epoch) {
		lockPeriod = staking.LockPeriodInEpochV2
	}
	return lockPeriod
}
