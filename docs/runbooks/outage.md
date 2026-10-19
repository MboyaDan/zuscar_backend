# Outage runbook (DRAFT, Stage 16)

When the internet, the API or the whole system is down, the venue keeps selling on paper and the sales are entered into the system afterwards. This page is the procedure for both halves. It is a draft: the owner reads it, changes it to fit how the venue really works, and the staff walkthrough (AC-17) is done once with the real receipt book.

Assumptions this draft makes (tell me if any is wrong): the venue has a **numbered, two-part receipt book** (top copy to the customer, carbon copy stays in the book), and the Business Manager is the person who enters receipts afterwards.

## 1. When this applies
- The connection indicator in the app shows the system as offline for more than a few minutes, or sign-in and the POS screen fail with "Something went wrong" on every try.
- A manager decides. Do not switch to paper for a single slow screen.

## 2. During the outage (cashiers, waiters, bartenders)
1. Take orders on paper pads. The kitchen cooks from the paper ticket.
2. For every sale, write a receipt from the numbered book: the **book number**, the date and time, each item with quantity and the **unit price actually charged**, the **total**, and how it was paid (cash, or the M-Pesa / card / bank reference).
3. Give the top copy to the customer. Keep the carbon in the book, in number order. Never tear a carbon out.
4. A receipt written wrongly is crossed out, marked VOID, signed, and left in the book. It is **not** entered into the system.
5. Cash stays in the drawer. Write nothing in the system, and do not re-key a sale as a new order when the system returns: that sells it twice.

## 3. After the outage (Business Manager)
1. Open the app, go to the **Outage entries** screen (Sales menu), and have the carbon copies in number order.
2. For each carbon, enter: the book number exactly as printed, when the sale happened, the items (product, quantity, the unit price on the paper), the payments (with the reference for M-Pesa, card or bank), and the total written on the paper.
3. If the total on the paper is not what the lines add up to, the screen asks for a note saying why. Write what you can see on the paper ("clerk wrote 150.00").
4. Submit. A receipt number already entered is refused with "Paper receipt ... was already entered". That is the guard doing its job: check whether you are holding the same carbon twice. Do not change the number to get past it.
5. When the last carbon is entered, count the entries against the book (first number to last number, minus the voided ones). The list on the same screen shows every entry with who entered it.
6. Mark each carbon in the book (a tick and the date). Keep the book.

## 4. The cash-up
The cash taken during the outage is in the drawer but belongs to no shift in the system, so the cash-up of the shift that held that drawer will show a **surplus** equal to the outage cash. When you reconcile that shift, write the reason in the note (for example "outage cash 18,400 entered from paper book B, receipts 0412 to 0431").

## 5. Mistakes and limits
- A paper receipt entered wrongly is corrected like any sale: refund the wrong lines, then enter nothing again. The entry itself is never edited or deleted.
- Receipts older than **31 days** are not accepted by the screen. Bring them to the owner.
- No receipt is printed for an entered sale; the customer already has the paper one.
- The entry needs the `payments.reconcile` permission. The cashier who wrote the paper receipt does not enter it.

## 6. Staff walkthrough (AC-17), to be done once
- [ ] The Business Manager enters three real or practice carbons, one with a total that differs from the lines.
- [ ] The same carbon is entered a second time and is refused.
- [ ] The count of entries matches the book.
- [ ] The cash-up note for the shift is written.
- [ ] A cashier signs in and cannot open the Outage entries screen.
