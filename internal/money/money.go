// Package money parses and formats amounts and currencies.
//
// All amounts are stored as int64 in hundredths (kopecks, cents) — for any currency.
package money

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxAmount guards against overflow and typos: 1 trillion.
const MaxAmount int64 = 1_000_000_000_000 * 100

var ErrInvalid = errors.New("некорректная сумма")

const nbsp = " "

var symbols = map[string]string{
	"RUB": "₽", "USD": "$", "EUR": "€", "GBP": "£", "KZT": "₸", "UAH": "₴",
	"GEL": "₾", "TRY": "₺", "AMD": "֏", "CNY": "¥", "THB": "฿", "AZN": "₼",
	"KRW": "₩", "INR": "₹", "ILS": "₪", "VND": "₫", "BYN": "Br",
}

var symbolRunes = map[rune]string{
	'₽': "RUB", '$': "USD", '€': "EUR", '£': "GBP", '₸': "KZT", '₴': "UAH",
	'₾': "GEL", '₺': "TRY", '֏': "AMD", '¥': "CNY", '฿': "THB", '₼': "AZN",
	'₩': "KRW", '₹': "INR", '₪': "ILS", '₫': "VND",
}

var aliases = map[string]string{
	"р": "RUB", "руб": "RUB", "рубль": "RUB", "рубля": "RUB", "рублей": "RUB", "rub": "RUB",
	"usd": "USD", "дол": "USD", "долл": "USD", "доллар": "USD", "доллара": "USD", "долларов": "USD", "бакс": "USD", "баксов": "USD",
	"eur": "EUR", "евро": "EUR",
	"тг": "KZT", "тенге": "KZT",
	"грн": "UAH", "гривна": "UAH", "гривен": "UAH",
	"лари": "GEL", "лира": "TRY", "лир": "TRY", "драм": "AMD", "юань": "CNY", "юаней": "CNY",
	"бат": "THB", "батов": "THB", "дирхам": "AED", "дирхамов": "AED",
}

// known lists the codes recognized in quick input ("20 chf taxi").
var known = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`RUB USD EUR GBP CNY JPY KZT UAH BYN GEL AMD AZN TRY AED THB KGS UZS TJS
		CHF CAD AUD NZD PLN CZK SEK NOK DKK ILS INR KRW VND IDR RSD HKD SGD MDL BGN HUF RON BRL MXN ARS
		MYR PHP EGP SAR QAR ZAR USDT USDC BTC ETH TON`) {
		known[c] = true
	}
}

var codeRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,5}$`)

// ValidCode checks the format of a currency code (ISO 4217 or a ticker such as USDT).
func ValidCode(code string) bool { return codeRe.MatchString(code) }

// Symbol returns the currency symbol, or the code itself if there is none.
func Symbol(code string) string {
	if s, ok := symbols[code]; ok {
		return s
	}
	return code
}

// ParseCurrency recognizes a currency by code, symbol or word ("usd", "$", "руб").
func ParseCurrency(token string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(token))
	t = strings.TrimSuffix(t, ".")
	if t == "" {
		return "", false
	}
	if r, size := utf8.DecodeRuneInString(t); size == len(t) {
		if code, ok := symbolRunes[r]; ok {
			return code, true
		}
	}
	if code, ok := aliases[t]; ok {
		return code, true
	}
	if code := strings.ToUpper(t); known[code] {
		return code, true
	}
	return "", false
}

// Parse parses an amount: "1500", "1 500,50", "1,234.56", "-200".
func Parse(s string) (int64, error) {
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "−"):
		neg, s = true, strings.TrimPrefix(s, "−")
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '\'' || r == '_' {
			return -1
		}
		return r
	}, s)
	s = normalizeSeparators(s)

	intPart, frac, hasFrac := strings.Cut(s, ".")
	if len(frac) > 2 && strings.Trim(frac[2:], "0") == "" {
		frac = frac[:2] // «0.500» → «0.50»
	}
	if intPart == "" {
		if !hasFrac {
			return 0, ErrInvalid
		}
		intPart = "0" // «.5»
	}
	if !isDigits(intPart) || (hasFrac && (frac == "" || len(frac) > 2 || !isDigits(frac))) {
		return 0, ErrInvalid
	}
	if len(intPart) > 13 {
		return 0, ErrInvalid
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrInvalid
	}
	var cents int64
	if hasFrac {
		cents, _ = strconv.ParseInt(frac, 10, 64)
		if len(frac) == 1 {
			cents *= 10
		}
	}
	v := whole*100 + cents
	if v > MaxAmount {
		return 0, ErrInvalid
	}
	if neg {
		v = -v
	}
	return v, nil
}

// normalizeSeparators normalizes separators to the "1234.56" form.
func normalizeSeparators(s string) string {
	commas, dots := strings.Count(s, ","), strings.Count(s, ".")
	switch {
	case commas > 0 && dots > 0:
		// The decimal separator is the last one: "1,234.56" / "1.234,56".
		if strings.LastIndex(s, ",") > strings.LastIndex(s, ".") {
			s = strings.ReplaceAll(s, ".", "")
			return strings.Replace(s, ",", ".", 1)
		}
		return strings.ReplaceAll(s, ",", "")
	case commas+dots > 1:
		// "1.234.567" has only thousands separators; groups must be 3 digits.
		groups := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '.' })
		if len(groups) != commas+dots+1 || len(groups[0]) > 3 {
			return "invalid"
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return "invalid"
			}
		}
		return strings.Join(groups, "")
	case commas+dots == 1:
		i := strings.IndexAny(s, ".,")
		if len(s)-i-1 == 3 && s[:i] != "0" && s[:i] != "" && len(s[:i]) <= 3 { // "1,500" is thousands
			return s[:i] + s[i+1:]
		}
		return s[:i] + "." + s[i+1:]
	}
	return s
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Input is the result of parsing quick input such as "+500 usd salary".
type Input struct {
	Sign     int // +1 for explicit "+", -1 for explicit "-", 0 if unsigned
	Amount   int64
	Currency string // empty if no currency was given
	Note     string
}

var amountRe = regexp.MustCompile(`^\d[\d\s.,']*`)

func ParseInput(s string) (Input, error) {
	var in Input
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "+"):
		in.Sign, s = 1, s[1:]
	case strings.HasPrefix(s, "-"):
		in.Sign, s = -1, s[1:]
	case strings.HasPrefix(s, "−"):
		in.Sign, s = -1, strings.TrimPrefix(s, "−")
	}
	s = strings.TrimSpace(s)

	// Currency symbol before the amount: "$20".
	if r, size := utf8.DecodeRuneInString(s); r != utf8.RuneError && !unicode.IsDigit(r) {
		code, ok := symbolRunes[r]
		if !ok {
			return in, ErrInvalid
		}
		in.Currency, s = code, strings.TrimSpace(s[size:])
	}

	m := amountRe.FindString(s)
	num := strings.TrimRight(m, " \t .,'")
	if num == "" {
		return in, ErrInvalid
	}
	amount, err := Parse(num)
	if err != nil || amount <= 0 {
		return in, ErrInvalid
	}
	in.Amount = amount
	rest := strings.TrimSpace(s[len(num):])
	rest = strings.TrimLeft(rest, ".,")
	rest = strings.TrimSpace(rest)

	// Currency after the amount: "20$", "20 usd", "20 руб.".
	if rest != "" && in.Currency == "" {
		if r, size := utf8.DecodeRuneInString(rest); symbolRunes[r] != "" {
			in.Currency, rest = symbolRunes[r], strings.TrimSpace(rest[size:])
		} else {
			word, tail := rest, ""
			if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
				word, tail = rest[:i], rest[i:]
			}
			if code, ok := ParseCurrency(word); ok {
				in.Currency, rest = code, strings.TrimSpace(tail)
			}
		}
	}
	in.Note = rest
	return in, nil
}

// Format: 1234550, "RUB" → «12 345,50 ₽».
func Format(amount int64, currency string) string {
	return Number(amount) + nbsp + Symbol(currency)
}

// FormatSigned adds "+" to positive amounts.
func FormatSigned(amount int64, currency string) string {
	if amount > 0 {
		return "+" + Format(amount, currency)
	}
	return Format(amount, currency)
}

// Number formats a number with digit grouping, without a currency.
func Number(amount int64) string {
	sign := ""
	if amount < 0 {
		sign, amount = "−", -amount
	}
	s := group(amount / 100)
	if frac := amount % 100; frac != 0 {
		s += "," + strconv.FormatInt(100+frac, 10)[1:]
	}
	return sign + s
}

// Plain is the CSV format: "-1234,56".
func Plain(amount int64) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	return sign + strconv.FormatInt(amount/100, 10) + "," + strconv.FormatInt(100+amount%100, 10)[1:]
}

func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head > 0 {
		b.WriteString(s[:head])
	}
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(nbsp)
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
