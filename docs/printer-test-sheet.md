# Printer test sheet (AC-16, DG-03)

Purpose: prove on the real printer, with the real browser and Windows setup, that the three print documents come out readable at 80 mm. This cannot be checked from the code: a browser cannot tell whether paper came out. Run it once when the printer arrives, again after any driver, browser or printer change, and keep the filled sheet.

Nothing here touches real sales. The test page makes a print job of type `test_page`; the receipt and kitchen ticket checks use fake data on a development or staging site, never the live venue data.

## Before you start
| Item | Write down |
|---|---|
| Printer model | |
| Connection (USB, network, Bluetooth) | |
| Windows version and driver name and version | |
| Browser and version (Chrome or Edge on Windows) | |
| Paper: width 80 mm thermal, roll size | |
| Site used (development or staging URL) | |

## Browser print settings to use every time
1. Destination: the thermal printer (not "Save as PDF").
2. Paper size: the 80 mm roll size the driver offers (for example "80mm x Receipt" or "Roll Paper 80 x 297 mm").
3. Margins: None (or Minimum). Scale: 100% (not "Fit to page"). Headers and footers: off. Background graphics: off.
4. If the browser's own dialog is skipped by kiosk printing, record how it is set up and who can change it.

## Checks
Tick each line, and write what you saw if it did not pass. Take a photo of each printout and keep it with this sheet.

### A. Test page (Printing → Print a test page)
| # | Check | Pass | Notes |
|---|---|---|---|
| A1 | The page prints on one piece of paper and the printer cuts or feeds correctly | | |
| A2 | The ruler line marked 72 mm measures 72 mm with a ruler (print area is about 72 mm of the 80 mm paper) | | |
| A3 | Nothing is cut off at the left or right edge; the left and right edge marks are both visible | | |
| A4 | The solid black box is solid black, with no grey streaks or white lines | | |
| A5 | The smallest text line is readable at arm's length | | |
| A6 | The logo prints sharp, black on white, not stretched | | |
| A7 | The digits 0 and 8, and 1 and 7, can be told apart in the number line | | |

### B. Receipt (any fake sale → Print receipt)
| # | Check | Pass | Notes |
|---|---|---|---|
| B0 | Sale page → Print receipt; the print window shows a preview first | | |
| B1 | Business name, receipt number, date and time, cashier are present | | |
| B2 | Quantities, names, line totals, total, tax-included line, payments, balance are present and line up on the right | | |
| B3 | No wording says "tax invoice" or claims KRA/eTIMS compliance (this version must not) | | |
| B4 | An unverified tax setup prints the "not verified" line | | |
| B5 | Print another copy ("Print another copy (REPRINT)" in the print window, or Open on the Printing page): it shows REPRINT and a copy number, large enough to see at a glance | | |

### C. Kitchen ticket (send an order to the kitchen → Print ticket)
| # | Check | Pass | Notes |
|---|---|---|---|
| C1 | The word KITCHEN is large at the top and the ticket cannot be mistaken for a receipt | | |
| C2 | Ticket number, table or takeaway, time and the waiter are present; there are no prices | | |
| C3 | A change to an order already sent prints "CHANGE" and cancelled lines say CANCEL | | |
| C4 | Notes ("no onions") print in full | | |

### D. Failure (AC-03)
| # | Check | Pass | Notes |
|---|---|---|---|
| D1 | Switch the printer off or unplug it, print a receipt, then in the dialog choose "It did not print" | | |
| D2 | The job shows Failed. The sale is still there, once, with the same receipt number | | |
| D3 | Switch the printer on, press Retry: one receipt prints, no second sale and no new number | | |
| D4 | Press Printed. The job shows Printed | | |

## Result
| Overall | Pass / Fail |
|---|---|
| Signed off by | |
| Date | |
| Problems to fix | |

If any line fails, do not go live on printing: report it with the photo and the settings used above.
