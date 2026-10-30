package money

import "testing"

func TestParse(t *testing.T) {
	cases := map[string]int64{
		"1500":      150000,
		"1 500":     150000,
		"1500,5":    150050,
		"1 500,50":  150050,
		"1,500":     150000,
		"1.234.567": 123456700,
		"1,234.56":  123456,
		"1.234,56":  123456,
		"0.99":      99,
		".5":        50,
		"-200":      -20000,
		"−15 000":   -1500000,
		"+10":       1000,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "1.5.5,123", "1e5", "10000000000000"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q): expected error", in)
		}
	}
}

func TestParseInput(t *testing.T) {
	cases := []struct {
		in   string
		want Input
	}{
		{"350 кофе", Input{Amount: 35000, Note: "кофе"}},
		{"+50000 зарплата", Input{Sign: 1, Amount: 5000000, Note: "зарплата"}},
		{"20$ такси", Input{Amount: 2000, Currency: "USD", Note: "такси"}},
		{"$20 такси", Input{Amount: 2000, Currency: "USD", Note: "такси"}},
		{"20 usd такси до дома", Input{Amount: 2000, Currency: "USD", Note: "такси до дома"}},
		{"1 500,50 обед", Input{Amount: 150050, Note: "обед"}},
		{"200 руб. хлеб", Input{Amount: 20000, Currency: "RUB", Note: "хлеб"}},
		{"15 евро", Input{Amount: 1500, Currency: "EUR"}},
		{"100 usdt", Input{Amount: 10000, Currency: "USDT"}},
		{"1000", Input{Amount: 100000}},
	}
	for _, c := range cases {
		got, err := ParseInput(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseInput(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"привет", "0", "кофе 350", "", "+"} {
		if _, err := ParseInput(in); err == nil {
			t.Errorf("ParseInput(%q): expected error", in)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		amount int64
		cur    string
		want   string
	}{
		{150000, "RUB", "1 500 ₽"},
		{123456789, "USD", "1 234 567,89 $"},
		{-5005, "EUR", "−50,05 €"},
		{100, "USDT", "1 USDT"},
	}
	for _, c := range cases {
		if got := Format(c.amount, c.cur); got != c.want {
			t.Errorf("Format(%d, %s) = %q; want %q", c.amount, c.cur, got, c.want)
		}
	}
	if got := Plain(-123405); got != "-1234,05" {
		t.Errorf("Plain = %q", got)
	}
}
