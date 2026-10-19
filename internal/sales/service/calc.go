package service

// All amounts are whole cents. Prices include tax (A-04), so the tax in a line is the share of its total that is
// tax: net * rate / (1 + rate), rounded half up to the cent, line by line. The rounding rule is a placeholder until
// the accountant confirms it (docs/tax-configuration.md item 4).

// inclusiveTax is the tax contained in net at rateBP basis points (100 basis points is 1%).
func inclusiveTax(net int64, rateBP int32) int64 {
	if net <= 0 || rateBP <= 0 {
		return 0
	}
	d := int64(10000 + rateBP)
	return (net*int64(rateBP)*2 + d) / (2 * d)
}

type lineMoney struct {
	Gross    int64 // (unit price + modifiers) * qty, before any discount
	Discount int64
	Net      int64 // the line total
	Tax      int64 // included in Net
}

func calcLine(qty int32, unit, mods, discount int64, rateBP int32) lineMoney {
	gross := (unit + mods) * int64(qty)
	net := gross - discount
	return lineMoney{Gross: gross, Discount: discount, Net: net, Tax: inclusiveTax(net, rateBP)}
}
