package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is an immutable monetary value. Amounts are stored as minor units
// (centavos for BRL), never as floating-point values.
type Money struct {
	minorUnits int64
	currency   string
}

// NewMoney creates a value for internal calculations. Negative values are
// allowed here because differences and reversals can be negative internally.
func NewMoney(minorUnits int64, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, currency)
	}
	return Money{minorUnits: minorUnits, currency: currency}, nil
}

// ParseMoney accepts a decimal with at most two fractional digits. It accepts
// an optional leading minus for internal calculations, but never exponent,
// NaN, Infinity, whitespace, or silent rounding.
func ParseMoney(amount, currency string) (Money, error) {
	minorUnits, err := parseMinorUnits(amount)
	if err != nil {
		return Money{}, err
	}
	return NewMoney(minorUnits, currency)
}

// ParseExternalMoney applies the stricter API/message boundary rule: an
// external financial amount cannot be negative.
func ParseExternalMoney(amount, currency string) (Money, error) {
	money, err := ParseMoney(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if money.IsNegative() {
		return Money{}, fmt.Errorf("%w: %s", ErrNegativeExternalAmount, amount)
	}
	return money, nil
}

func (m Money) MinorUnits() int64 { return m.minorUnits }

func (m Money) Currency() string { return m.currency }

func (m Money) IsZero() bool { return m.minorUnits == 0 }

func (m Money) IsNegative() bool { return m.minorUnits < 0 }

func (m Money) IsPositive() bool { return m.minorUnits > 0 }

func (m Money) String() string {
	negative := m.minorUnits < 0
	units := m.minorUnits
	if negative {
		// -(MinInt64) overflows, so format its magnitude without negating it.
		if units == math.MinInt64 {
			return "-92233720368547758.08"
		}
		units = -units
	}
	text := strconv.FormatInt(units/100, 10) + "." + fmt.Sprintf("%02d", units%100)
	if negative {
		return "-" + text
	}
	return text
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.sameCurrency(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits > math.MaxInt64-other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits < math.MinInt64-other.minorUnits) {
		return Money{}, ErrMoneyOverflow
	}
	return NewMoney(m.minorUnits+other.minorUnits, m.currency)
}

func (m Money) Subtract(other Money) (Money, error) {
	negated, err := other.Negate()
	if err != nil {
		return Money{}, err
	}
	return m.Add(negated)
}

func (m Money) Negate() (Money, error) {
	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrMoneyOverflow
	}
	return NewMoney(-m.minorUnits, m.currency)
}

// Compare returns -1, 0, or 1. Comparing distinct currencies is an error.
func (m Money) Compare(other Money) (int, error) {
	if err := m.sameCurrency(other); err != nil {
		return 0, err
	}
	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{Amount: m.String(), Currency: m.currency})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed, err := ParseExternalMoney(raw.Amount, raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func (m Money) sameCurrency(other Money) error {
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, character := range currency {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func parseMinorUnits(amount string) (int64, error) {
	if amount == "" || strings.TrimSpace(amount) != amount {
		return 0, fmt.Errorf("%w: amount must not be empty or contain whitespace", ErrInvalidAmount)
	}

	negative := false
	if amount[0] == '-' {
		negative = true
		amount = amount[1:]
	}
	if amount == "" {
		return 0, fmt.Errorf("%w: missing digits", ErrInvalidAmount)
	}

	parts := strings.Split(amount, ".")
	if len(parts) > 2 || parts[0] == "" || !allDigits(parts[0]) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if fraction == "" || len(fraction) > 2 || !allDigits(fraction) {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
		}
	}
	for len(fraction) < 2 {
		fraction += "0"
	}

	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrMoneyOverflow, err)
	}
	cents, _ := strconv.ParseInt(fraction, 10, 64)
	maxWhole := int64(math.MaxInt64 / 100)
	maxCents := int64(math.MaxInt64 % 100)
	if whole > maxWhole || (whole == maxWhole && cents > maxCents) {
		// The negative range has one additional representable value:
		// -92233720368547758.08 is math.MinInt64 in minor units.
		if negative && whole == maxWhole && cents == maxCents+1 {
			return math.MinInt64, nil
		}
		return 0, ErrMoneyOverflow
	}
	minorUnits := whole*100 + cents
	if negative {
		minorUnits = -minorUnits
	}
	return minorUnits, nil
}

func allDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
