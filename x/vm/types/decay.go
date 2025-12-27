package types

import (
	"github.com/holiman/uint256"
)

// Decay rate constants
// Rate is 0.000003171% per second = 3.171 × 10^-8 per second
// Represented as a fraction: 3171 / 10^13
var (
	// DecayRateNumerator is the numerator for the decay rate calculation
	DecayRateNumerator = uint256.NewInt(3171)
	// DecayRateDenominator is the denominator for the decay rate (10^13)
	DecayRateDenominator = new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(13))
)

// CalculateDecay computes the decay amount for a given balance and elapsed time.
// Formula: decay = balance × secondsElapsed × 3171 / 10^13
// This represents a 0.000003171% decay per second.
func CalculateDecay(balance *uint256.Int, secondsElapsed uint64) *uint256.Int {
	if balance == nil || balance.IsZero() || secondsElapsed == 0 {
		return uint256.NewInt(0)
	}

	// decay = (balance × seconds × 3171) / 10^13
	// Using intermediate steps to handle potential overflow

	// Step 1: balance × seconds
	seconds := uint256.NewInt(secondsElapsed)
	intermediate := new(uint256.Int).Mul(balance, seconds)

	// Step 2: multiply by rate numerator
	intermediate = intermediate.Mul(intermediate, DecayRateNumerator)

	// Step 3: divide by rate denominator
	decay := new(uint256.Int).Div(intermediate, DecayRateDenominator)

	return decay
}

// ApplyDecay returns the new balance after applying decay.
// Returns the decayed balance (original balance - decay amount).
// If decay exceeds balance, returns zero.
func ApplyDecay(balance *uint256.Int, secondsElapsed uint64) *uint256.Int {
	if balance == nil || balance.IsZero() {
		return uint256.NewInt(0)
	}

	decay := CalculateDecay(balance, secondsElapsed)

	// If decay is greater than or equal to balance, return zero
	if decay.Cmp(balance) >= 0 {
		return uint256.NewInt(0)
	}

	// Return balance - decay
	return new(uint256.Int).Sub(balance, decay)
}
