# Content still needed

- Business details (DQ-01): real name, address, phone, receipt header/footer, tax PIN. Placeholder "Zuscar Venue" until supplied.
- Logo set received (7 files in uploads): favicon SVG, icon, reversed icon, horizontal, reversed horizontal, one-colour, 1-bit receipt logo PNG (384x109).
  - favicon PNGs at 16, 32 and 64 px (public/brand) and the 180 px Apple icon (app/apple-icon.png, placed on Cream because iOS fills transparency with black) were RENDERED from the supplied favicon SVG at Stage 4 by `npm run brand:icons`. Replace them if you have designer-made PNGs.
  - Note: the SVGs carry embedded C2PA metadata blocks; harmless, but they add bytes. Files will be used as supplied (not redrawn or recoloured). Stage 4: all six SVG variants were rendered onto their intended backgrounds and checked by eye: horizontal on Cream, reversed lockup and reversed icon on Midnight, one-colour in black, icon and favicon as named. The receipt PNG is 1-bit, 384x109.
- Real menu, tables, rooms and rates, stock items/units/opening balances, suppliers, staff: entered through admin screens at go-live, not from source.
- Printer model, connection and Windows setup (DG-03).
- Accountant answers: VAT/levy treatment, receipt numbering gaps, eTIMS obligation (DQ-11, DQ-12, DG-01).
- M-Pesa account type and access (DG-02).
- Staff training material (written at Stage 26).
