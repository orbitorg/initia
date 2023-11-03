package keeper

import (
	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	distrtypes "github.com/initia-labs/initia/x/distribution/types"
	"github.com/initia-labs/initia/x/move/types"
	stakingtypes "github.com/initia-labs/initia/x/mstaking/types"
	vmtypes "github.com/initia-labs/initiavm/types"
)

// AmountToShare convert token to share in the ratio of a validator's share/token
func (k Keeper) AmountToShare(ctx sdk.Context, valAddr sdk.ValAddress, amount sdk.Coin) (math.Int, error) {
	val := k.StakingKeeper.Validator(ctx, valAddr)
	if val == nil {
		return sdk.ZeroInt(), stakingtypes.ErrNoValidatorFound
	}

	shares, err := val.SharesFromTokens(sdk.NewCoins(amount))
	if err != nil {
		return sdk.ZeroInt(), err
	}

	return shares.AmountOf(amount.Denom).TruncateInt(), err
}

// ShareToAmount convert share to token in the ratio of a validator's token/share
func (k Keeper) ShareToAmount(ctx sdk.Context, valAddr sdk.ValAddress, share sdk.DecCoin) (math.Int, error) {
	val := k.StakingKeeper.Validator(ctx, valAddr)
	if val == nil {
		return sdk.ZeroInt(), stakingtypes.ErrNoValidatorFound
	}

	tokens := val.TokensFromShares(sdk.NewDecCoins(share))
	return tokens.AmountOf(share.Denom).TruncateInt(), nil
}

// WithdrawRewards withdraw rewards from a validator and send the
// withdrawn staking rewards to the move staking module account
func (k Keeper) WithdrawRewards(ctx sdk.Context, valAddr sdk.ValAddress) (distrtypes.Pools, error) {
	delModuleAddr := types.GetDelegatorModuleAddress(valAddr)
	if k.hasZeroRewards(ctx, valAddr, delModuleAddr) {
		return nil, nil
	}

	rewardPools, err := k.distrKeeper.WithdrawDelegationRewards(ctx, delModuleAddr, valAddr)
	if err != nil {
		return nil, err
	}

	// move staking only support reward denom
	rewardDenom := k.RewardKeeper.GetParams(ctx).RewardDenom

	pools := make(distrtypes.Pools, 0, len(rewardPools))
	for _, pool := range rewardPools {
		rewardAmount := pool.Coins.AmountOf(rewardDenom)
		if rewardAmount.IsPositive() {
			pools = append(pools, distrtypes.NewPool(
				pool.Denom,
				sdk.NewCoins(sdk.NewCoin(rewardDenom, rewardAmount)),
			))
		}
	}

	// send other rewards except mint denom to community pool
	otherRewards := rewardPools.Sub(pools).Sum()
	if !otherRewards.IsZero() {
		err = k.communityPoolKeeper.FundCommunityPool(ctx, otherRewards, delModuleAddr)
		if err != nil {
			return nil, err
		}
	}

	// send all rewards to move staking module account
	err = k.bankKeeper.SendCoinsFromAccountToModule(ctx, delModuleAddr, types.MoveStakingModuleName, pools.Sum())
	return pools, err
}

// check whether a delegation rewards is zero or not with cache context
// to prevent write operation at checking
func (k Keeper) hasZeroRewards(ctx sdk.Context, validatorAddr sdk.ValAddress, delegatorAddr sdk.AccAddress) bool {
	ctx, _ = ctx.CacheContext()

	val := k.StakingKeeper.Validator(ctx, validatorAddr)
	if val == nil {
		return true
	}

	del := k.StakingKeeper.Delegation(ctx, delegatorAddr, validatorAddr)
	if del == nil {
		return true
	}
	endingPeriod := k.distrKeeper.IncrementValidatorPeriod(ctx, val)
	rewards, _ := k.distrKeeper.CalculateDelegationRewards(ctx, val, del, endingPeriod).TruncateDecimal()

	return rewards.IsEmpty()
}

// DelegateToValidator withdraw staking coins from the move module account
// and send the coins to a delegator module account for a validator and
// consequentially delegate the deposited coins to a validator.
func (k Keeper) DelegateToValidator(ctx sdk.Context, valAddr sdk.ValAddress, delCoins sdk.Coins) (sdk.DecCoins, error) {
	delegatorModuleName := types.GetDelegatorModuleName(valAddr)
	macc := k.authKeeper.GetModuleAccount(ctx, delegatorModuleName)

	// register module account if not registered
	if macc == nil {
		macc = authtypes.NewEmptyModuleAccount(delegatorModuleName)
		k.authKeeper.SetModuleAccount(ctx, macc)
	}

	delModuleAddr := macc.GetAddress()

	// send staking coin move module to validator module account
	// delegated coins are burned, so we should mint coins to module account
	err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.MoveStakingModuleName, delModuleAddr, delCoins)
	if err != nil {
		return sdk.NewDecCoins(), err
	}

	// delegate to validator
	val, found := k.StakingKeeper.GetValidator(ctx, valAddr)
	if !found {
		return sdk.NewDecCoins(), stakingtypes.ErrNoValidatorFound
	}

	shares, err := k.StakingKeeper.Delegate(ctx, delModuleAddr, delCoins, stakingtypes.Unbonded, val, true)
	return shares, err
}

// InstantUnbondFromValidator unbond coins without unbonding period and send
// the withdrawn coins to the move module account
func (k Keeper) InstantUnbondFromValidator(ctx sdk.Context, valAddr sdk.ValAddress, shares sdk.DecCoins) (sdk.Coins, error) {
	val, found := k.StakingKeeper.GetValidator(ctx, valAddr)
	if !found {
		return sdk.NewCoins(), stakingtypes.ErrNoValidatorFound
	}

	// unbond from a validator
	delModuleAddr := types.GetDelegatorModuleAddress(valAddr)
	returnCoins, err := k.StakingKeeper.Unbond(ctx, delModuleAddr, valAddr, shares)
	if err != nil {
		return sdk.NewCoins(), err
	}

	if val.IsBonded() {
		err = k.bankKeeper.SendCoinsFromModuleToModule(ctx, stakingtypes.BondedPoolName, types.MoveStakingModuleName, returnCoins)
	} else {
		err = k.bankKeeper.SendCoinsFromModuleToModule(ctx, stakingtypes.NotBondedPoolName, types.MoveStakingModuleName, returnCoins)
	}

	return returnCoins, err
}

// ApplyStakingDeltas iterate staking deltas to increase or decrease
// a staking amount, and deposit unbonding coin to staking contract.
func (k Keeper) ApplyStakingDeltas(
	ctx sdk.Context,
	stakingDeltas []vmtypes.StakingDelta,
) error {
	delegations := make(map[string]sdk.Coins)
	undelegations := make(map[string]sdk.DecCoins)
	for _, delta := range stakingDeltas {
		valAddrStr := string(delta.Validator)
		if _, found := delegations[valAddrStr]; !found {
			delegations[valAddrStr] = sdk.NewCoins()
		}
		if _, found := undelegations[valAddrStr]; !found {
			undelegations[valAddrStr] = sdk.NewDecCoins()
		}

		denom, err := types.DenomFromMetadataAddress(ctx, NewMoveBankKeeper(&k), delta.Metadata)
		if err != nil {
			return err
		}

		if delta.Delegation > 0 {
			delCoin := sdk.NewCoin(denom, sdk.NewIntFromUint64(delta.Delegation))
			delegations[valAddrStr] = delegations[valAddrStr].Add(delCoin)
		}

		if delta.Undelegation > 0 {
			undelCoin := sdk.NewDecCoin(denom, sdk.NewIntFromUint64(delta.Undelegation))
			undelegations[valAddrStr] = undelegations[valAddrStr].Add(undelCoin)
		}
	}

	for valAddrStr, delegationCoins := range delegations {
		if !delegationCoins.IsZero() {
			valAddr, err := sdk.ValAddressFromBech32(valAddrStr)
			if err != nil {
				return err
			}

			_, err = k.DelegateToValidator(ctx, valAddr, delegationCoins)
			if err != nil {
				return err
			}
		}
	}

	amountVecMap := make(map[string][]uint64)
	valAddrVecMap := make(map[string][][]byte)
	for valAddrStr, undelegationShares := range undelegations {
		if !undelegationShares.IsZero() {
			valAddr, err := sdk.ValAddressFromBech32(valAddrStr)
			if err != nil {
				return err
			}

			unbondingAmount, err := k.InstantUnbondFromValidator(ctx, valAddr, undelegationShares)
			if err != nil {
				return err
			}

			// build maps for `deposit_unbonding_coin` execution
			for _, amount := range unbondingAmount {
				if amount.IsZero() {
					continue
				}

				amountVecMap[amount.Denom] = append(amountVecMap[amount.Denom], amount.Amount.Uint64())
				valAddrVecMap[amount.Denom] = append(valAddrVecMap[amount.Denom], []byte(valAddrStr))
			}
		}
	}

	for unbondingDenom := range amountVecMap {
		err := k.DepositUnbondingCoins(ctx, unbondingDenom, amountVecMap[unbondingDenom], valAddrVecMap[unbondingDenom])
		if err != nil {
			return err
		}

	}

	return nil
}

// DepositUnbondingCoin deposit instantly unbonded coins to staking contract
func (k Keeper) DepositUnbondingCoins(
	ctx sdk.Context,
	unbondingDenom string,
	unbondingAmounts []uint64,
	valAddrs [][]byte,
) error {
	amountArg, err := vmtypes.SerializeUint64Vector(unbondingAmounts)
	if err != nil {
		return err
	}

	valArg, err := vmtypes.SerializeBytesVector(valAddrs)
	if err != nil {
		return err
	}

	metadata, err := types.MetadataAddressFromDenom(unbondingDenom)
	if err != nil {
		return err
	}

	args := [][]byte{metadata[:], valArg, amountArg}
	return k.ExecuteEntryFunction(
		ctx,
		vmtypes.StdAddress,
		vmtypes.StdAddress,
		types.MoveModuleNameStaking,
		types.FunctionNameStakingDepositUnbondingCoin,
		[]vmtypes.TypeTag{},
		args,
	)
}

func (k Keeper) GetStakingStatesTableHandle(ctx sdk.Context) (vmtypes.AccountAddress, error) {
	res, err := k.GetResourceBytes(ctx, vmtypes.StdAddress, vmtypes.StructTag{
		Address:  vmtypes.StdAddress,
		Module:   types.MoveModuleNameStaking,
		Name:     types.ResourceNameModuleStore,
		TypeArgs: []vmtypes.TypeTag{},
	})
	if err != nil {
		return vmtypes.AccountAddress{}, err
	}

	return types.ReadStakingStatesTableHandleFromModuleStore(res)
}

// HasStakingState return the flag whether the metadata has registered as staking denom.
func (k Keeper) HasStakingState(ctx sdk.Context, metadata vmtypes.AccountAddress) (bool, error) {
	stakingStatesTableHandle, err := k.GetStakingStatesTableHandle(ctx)
	if err != nil {
		return false, err
	}

	return k.HasTableEntry(ctx, stakingStatesTableHandle, metadata[:]), nil
}

// SlashUnbondingCoin slash unbonding coins of the staking contract
func (k Keeper) SlashUnbondingDelegations(
	ctx sdk.Context,
	valAddr sdk.ValAddress,
	fraction sdk.Dec,
) error {
	stakingStatesTableHandle, err := k.GetStakingStatesTableHandle(ctx)
	if err != nil {
		return err
	}

	bondDenoms := k.StakingKeeper.BondDenoms(ctx)
	metadatas := make([]vmtypes.AccountAddress, 0, len(bondDenoms))
	for _, bondDenom := range bondDenoms {
		metadata, err := types.MetadataAddressFromDenom(bondDenom)
		if err != nil {
			return err
		}

		// check whether there is staking state for the given denom
		if !k.HasTableEntry(ctx, stakingStatesTableHandle, metadata[:]) {
			continue
		}

		// read metadata entry
		tableEntry, err := k.GetTableEntryBytes(ctx, stakingStatesTableHandle, metadata[:])
		if err != nil {
			return err
		}

		// metadata table handle
		metadataTableHandle, err := types.ReadTableHandleFromTable(tableEntry.ValueBytes)
		if err != nil {
			return err
		}

		// check whether the validator has non-zero unbonding balances
		keyBz, err := vmtypes.SerializeString(valAddr.String())
		if err != nil {
			return err
		}

		// check whether there is staking state for the validator
		if !k.HasTableEntry(ctx, metadataTableHandle, keyBz) {
			continue
		}

		// read validator entry
		tableEntry, err = k.GetTableEntry(ctx, metadataTableHandle, keyBz)
		if err != nil {
			return err
		}

		_, unbondingCoinStore, err := types.ReadUnbondingInfosFromStakingState(tableEntry.ValueBytes)
		if err != nil {
			return err
		}

		_, unbondingAmount, err := NewMoveBankKeeper(&k).Balance(ctx, unbondingCoinStore)
		if err != nil {
			return err
		}

		if unbondingAmount.IsPositive() {
			metadatas = append(metadatas, metadata)
		}
	}

	for _, metadata := range metadatas {
		fractionArg, err := vmtypes.SerializeString(fraction.String())
		if err != nil {
			return err
		}

		valArg, err := vmtypes.SerializeString(valAddr.String())
		if err != nil {
			return err
		}

		args := [][]byte{metadata[:], valArg, fractionArg}
		if err := k.ExecuteEntryFunction(
			ctx,
			vmtypes.StdAddress,
			vmtypes.StdAddress,
			types.MoveModuleNameStaking,
			types.FunctionNameStakingSlashUnbondingCoin,
			[]vmtypes.TypeTag{},
			args,
		); err != nil {
			return err
		}
	}

	return nil
}

// make staking states table for the given denom
func (k Keeper) InitializeStaking(
	ctx sdk.Context,
	bondDenom string,
) error {
	metadata, err := types.MetadataAddressFromDenom(bondDenom)
	if err != nil {
		return err
	}

	return k.InitializeStakingWithMetadata(ctx, metadata)
}

// make staking states table for the given metadata
func (k Keeper) InitializeStakingWithMetadata(
	ctx sdk.Context,
	metadata vmtypes.AccountAddress,
) error {
	if err := k.ExecuteEntryFunction(
		ctx,
		vmtypes.StdAddress,
		vmtypes.StdAddress,
		types.MoveModuleNameStaking,
		types.FunctionNameStakingInitialize,
		[]vmtypes.TypeTag{},
		[][]byte{metadata[:]},
	); err != nil {
		return err
	}

	return nil
}