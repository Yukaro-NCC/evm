package keeper

import (
	"encoding/binary"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/store/prefix"
	storetypes "cosmossdk.io/store/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

var _ statedb.Keeper = &Keeper{}

// ----------------------------------------------------------------------------
// StateDB Keeper implementation
// ----------------------------------------------------------------------------

// GetAccount returns nil if account does not exist.
// This method applies lazy balance decay based on elapsed time since last access.
// Decay rate: 0.000003171% per second of the balance.
//
// Decay is calculated for all contexts (queries, simulations, etc.) to show correct balances,
// but the actual burning of tokens and timestamp updates only occur during transaction
// execution (ExecModeFinalize) to avoid state changes during read-only operations.
func (k *Keeper) GetAccount(ctx sdk.Context, addr common.Address) *statedb.Account {
	acct := k.GetAccountWithoutBalance(ctx, addr)
	if acct == nil {
		return nil
	}

	balance := k.SpendableCoin(ctx, addr)
	if balance == nil || balance.IsZero() {
		acct.Balance = balance
		return acct
	}

	// Get decay timestamp to calculate elapsed time
	currentTime := uint64(ctx.BlockTime().Unix()) //nolint:gosec // G115 // won't exceed uint64
	lastDecayTime := k.GetDecayTimestamp(ctx, addr)

	// If no decay timestamp set yet, the balance is the actual balance
	// We only initialize the timestamp during transaction execution
	if lastDecayTime == 0 {
		// Only set initial timestamp during actual transaction execution
		if ctx.ExecMode() == sdk.ExecModeFinalize {
			k.SetDecayTimestamp(ctx, addr, currentTime)
		}
		acct.Balance = balance
		return acct
	}

	if currentTime <= lastDecayTime {
		// No time elapsed or clock skew, no decay
		acct.Balance = balance
		return acct
	}

	// Calculate elapsed seconds and compute decayed balance
	elapsedSeconds := currentTime - lastDecayTime
	decayedBalance := types.ApplyDecay(balance, elapsedSeconds)

	// Only apply state changes (burning, timestamp update) during actual transaction execution
	// This prevents state modifications during queries, simulations, CheckTx, etc.
	if ctx.ExecMode() == sdk.ExecModeFinalize {
		// If decay occurred, burn the difference
		if decayedBalance.Cmp(balance) < 0 {
			decayAmount := new(uint256.Int).Sub(balance, decayedBalance)
			if !decayAmount.IsZero() {
				// Burn the decayed amount from the account
				if err := k.burnDecayedAmount(ctx, addr, decayAmount); err != nil {
					// Log the error but continue with the original balance
					k.Logger(ctx).Error(
						"failed to burn decayed balance",
						"address", addr.Hex(),
						"decay_amount", decayAmount.String(),
						"error", err.Error(),
					)
					acct.Balance = balance
					return acct
				}
			}
		}
		// Update the decay timestamp
		k.SetDecayTimestamp(ctx, addr, currentTime)
	}

	acct.Balance = decayedBalance
	return acct
}

// burnDecayedAmount burns the decayed amount from an account's balance.
func (k *Keeper) burnDecayedAmount(ctx sdk.Context, addr common.Address, amount *uint256.Int) error {
	cosmosAddr := sdk.AccAddress(addr.Bytes())
	return k.bankWrapper.BurnAmountFromAccount(ctx, cosmosAddr, amount.ToBig())
}

// GetDecayedBalance returns the balance with decay applied for the given address.
// This is a read-only calculation that does NOT modify state (no burning, no timestamp update).
// Use this for queries like eth_getBalance.
func (k *Keeper) GetDecayedBalance(ctx sdk.Context, addr common.Address) *uint256.Int {
	balance := k.SpendableCoin(ctx, addr)
	if balance == nil || balance.IsZero() {
		return balance
	}

	// Get decay timestamp to calculate elapsed time
	currentTime := uint64(ctx.BlockTime().Unix()) //nolint:gosec // G115 // won't exceed uint64
	lastDecayTime := k.GetDecayTimestamp(ctx, addr)

	// If no decay timestamp set yet, return current balance (no decay reference point)
	if lastDecayTime == 0 {
		return balance
	}

	if currentTime <= lastDecayTime {
		// No time elapsed or clock skew, no decay
		return balance
	}

	// Calculate elapsed seconds and compute decayed balance
	elapsedSeconds := currentTime - lastDecayTime
	return types.ApplyDecay(balance, elapsedSeconds)
}

// GetState loads contract state from database.
func (k *Keeper) GetState(ctx sdk.Context, addr common.Address, key common.Hash) common.Hash {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.AddressStoragePrefix(addr))

	value := store.Get(key.Bytes())
	if len(value) == 0 {
		return common.Hash{}
	}

	return common.BytesToHash(value)
}

// GetFastState loads contract state from database.
func (k *Keeper) GetFastState(ctx sdk.Context, addr common.Address, key common.Hash) []byte {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.AddressStoragePrefix(addr))

	return store.Get(key.Bytes())
}

// GetCodeHash loads the code hash from the database for the given contract address.
func (k *Keeper) GetCodeHash(ctx sdk.Context, addr common.Address) common.Hash {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCodeHash)
	bz := store.Get(addr.Bytes())
	if len(bz) == 0 {
		return common.BytesToHash(types.EmptyCodeHash)
	}

	return common.BytesToHash(bz)
}

// IterateContracts iterates over all smart contract addresses in the EVM keeper and
// performs a callback function.
//
// The iteration is stopped when the callback function returns true.
func (k Keeper) IterateContracts(ctx sdk.Context, cb func(addr common.Address, codeHash common.Hash) (stop bool)) {
	store := ctx.KVStore(k.storeKey)
	iterator := storetypes.KVStorePrefixIterator(store, types.KeyPrefixCodeHash)

	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		addr := common.BytesToAddress(iterator.Key())
		codeHash := common.BytesToHash(iterator.Value())

		if cb(addr, codeHash) {
			break
		}
	}
}

// GetCode loads contract code from database, implements `statedb.Keeper` interface.
func (k *Keeper) GetCode(ctx sdk.Context, codeHash common.Hash) []byte {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCode)
	return store.Get(codeHash.Bytes())
}

// ForEachStorage iterate contract storage, callback return false to break early
func (k *Keeper) ForEachStorage(ctx sdk.Context, addr common.Address, cb func(key, value common.Hash) bool) {
	store := ctx.KVStore(k.storeKey)
	prefix := types.AddressStoragePrefix(addr)

	iterator := storetypes.KVStorePrefixIterator(store, prefix)
	defer iterator.Close()

	for ; iterator.Valid(); iterator.Next() {
		key := common.BytesToHash(iterator.Key())
		value := common.BytesToHash(iterator.Value())

		// check if iteration stops
		if !cb(key, value) {
			return
		}
	}
}

// SetBalance update account's balance, compare with current balance first, then decide to mint or burn.
func (k *Keeper) SetBalance(ctx sdk.Context, addr common.Address, amount *uint256.Int) error {
	if amount == nil {
		return nil
	}
	cosmosAddr := sdk.AccAddress(addr.Bytes())
	coin := k.bankWrapper.SpendableCoin(ctx, cosmosAddr, types.GetEVMCoinDenom())

	balance := coin.Amount.BigInt()
	delta := new(big.Int).Sub(amount.ToBig(), balance)
	switch delta.Sign() {
	case 1:
		// mint
		if err := k.bankWrapper.MintAmountToAccount(ctx, cosmosAddr, delta); err != nil {
			return err
		}
	case -1:
		// burn
		if err := k.bankWrapper.BurnAmountFromAccount(ctx, cosmosAddr, new(big.Int).Neg(delta)); err != nil {
			return err
		}
	default:
		// not changed
	}
	return nil
}

// SetAccount updates nonce/balance/codeHash together.
func (k *Keeper) SetAccount(ctx sdk.Context, addr common.Address, account statedb.Account) error {
	// update account
	acct := k.accountKeeper.GetAccount(ctx, addr.Bytes())
	if acct == nil {
		acct = k.accountKeeper.NewAccountWithAddress(ctx, addr.Bytes())
	}

	if err := acct.SetSequence(account.Nonce); err != nil {
		return err
	}

	if types.IsEmptyCodeHash(account.CodeHash) {
		k.DeleteCodeHash(ctx, addr)
	} else {
		k.SetCodeHash(ctx, addr.Bytes(), account.CodeHash)
	}
	k.accountKeeper.SetAccount(ctx, acct)

	if err := k.SetBalance(ctx, addr, account.Balance); err != nil {
		return err
	}

	k.Logger(ctx).Debug(
		"account updated",
		"ethereum-address", addr.Hex(),
		"nonce", account.Nonce,
		"codeHash", common.BytesToHash(account.CodeHash).Hex(),
		"balance", account.Balance,
	)
	return nil
}

// SetState update contract storage.
func (k *Keeper) SetState(ctx sdk.Context, addr common.Address, key common.Hash, value []byte) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.AddressStoragePrefix(addr))
	store.Set(key.Bytes(), value)

	k.Logger(ctx).Debug(
		"state updated",
		"ethereum-address", addr.Hex(),
		"key", key.Hex(),
	)
}

// DeleteState deletes the entry for the given key in the contract storage
// at the defined contract address.
func (k *Keeper) DeleteState(ctx sdk.Context, addr common.Address, key common.Hash) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.AddressStoragePrefix(addr))
	store.Delete(key.Bytes())

	k.Logger(ctx).Debug(
		"state deleted",
		"ethereum-address", addr.Hex(),
		"key", key.Hex(),
	)
}

// SetCodeHash sets the code hash for the given contract address.
func (k *Keeper) SetCodeHash(ctx sdk.Context, addrBytes, hashBytes []byte) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCodeHash)
	store.Set(addrBytes, hashBytes)

	k.Logger(ctx).Debug(
		"code hash updated",
		"address", common.BytesToAddress(addrBytes).Hex(),
		"code hash", common.BytesToHash(hashBytes).Hex(),
	)
}

// DeleteCodeHash deletes the code hash for the given contract address from the store.
func (k *Keeper) DeleteCodeHash(ctx sdk.Context, addr common.Address) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCodeHash)
	store.Delete(addr.Bytes())

	k.Logger(ctx).Debug(
		"code hash deleted",
		"address", addr.Hex(),
	)
}

// SetCode sets the given contract code bytes for the corresponding code hash bytes key
// in the code store.
func (k *Keeper) SetCode(ctx sdk.Context, codeHash, code []byte) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCode)
	store.Set(codeHash, code)

	k.Logger(ctx).Debug(
		"code updated",
		"code-hash", common.BytesToHash(codeHash).Hex(),
	)
}

// DeleteCode deletes the contract code for the given code hash bytes in
// the corresponding store.
func (k *Keeper) DeleteCode(ctx sdk.Context, codeHash []byte) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixCode)
	store.Delete(codeHash)

	k.Logger(ctx).Debug(
		"code deleted",
		"code-hash", common.BytesToHash(codeHash).Hex(),
	)
}

// DeleteAccount handles contract's suicide call:
// - clear balance
// - remove code
// - remove states
// - remove the code hash
// - remove auth account
func (k *Keeper) DeleteAccount(ctx sdk.Context, addr common.Address) error {
	cosmosAddr := sdk.AccAddress(addr.Bytes())
	acct := k.accountKeeper.GetAccount(ctx, cosmosAddr)
	if acct == nil {
		return nil
	}

	// NOTE: only Ethereum contracts can be self-destructed
	if !k.IsContract(ctx, addr) {
		return errors.New("only smart contracts can be self-destructed")
	}

	// set account to a base account to set the whole balance as spendable
	baseAccount := k.accountKeeper.GetAccount(ctx, cosmosAddr)
	k.accountKeeper.SetAccount(ctx, authtypes.NewBaseAccount(cosmosAddr, baseAccount.GetPubKey(), baseAccount.GetAccountNumber(), baseAccount.GetSequence()))

	// clear balance
	if err := k.SetBalance(ctx, addr, new(uint256.Int)); err != nil {
		return err
	}

	var keys []common.Hash

	// clear storage
	k.ForEachStorage(ctx, addr, func(key, _ common.Hash) bool {
		keys = append(keys, key)
		return true
	})

	for _, key := range keys {
		k.DeleteState(ctx, addr, key)
	}

	// clear code hash
	k.DeleteCodeHash(ctx, addr)

	// clear decay timestamp
	k.DeleteDecayTimestamp(ctx, addr)

	// remove auth account
	k.accountKeeper.RemoveAccount(ctx, acct)

	k.Logger(ctx).Debug(
		"account suicided",
		"ethereum-address", addr.Hex(),
		"cosmos-address", cosmosAddr.String(),
	)

	return nil
}

// ----------------------------------------------------------------------------
// Balance Decay
// ----------------------------------------------------------------------------

// GetDecayTimestamp returns the last access timestamp for an account.
// Returns 0 if not set (account never accessed for decay purposes).
func (k *Keeper) GetDecayTimestamp(ctx sdk.Context, addr common.Address) uint64 {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixDecayTimestamp)
	bz := store.Get(addr.Bytes())
	if len(bz) == 0 {
		return 0
	}
	return binary.BigEndian.Uint64(bz)
}

// SetDecayTimestamp sets the last access timestamp for an account.
func (k *Keeper) SetDecayTimestamp(ctx sdk.Context, addr common.Address, timestamp uint64) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixDecayTimestamp)
	bz := make([]byte, 8)
	binary.BigEndian.PutUint64(bz, timestamp)
	store.Set(addr.Bytes(), bz)
}

// DeleteDecayTimestamp removes the decay timestamp for an account.
func (k *Keeper) DeleteDecayTimestamp(ctx sdk.Context, addr common.Address) {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyPrefixDecayTimestamp)
	store.Delete(addr.Bytes())
}
