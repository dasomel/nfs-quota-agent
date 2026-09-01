# DESIGN.md — nfs-quota-agent dashboard

Design tokens and rationale for the web UI, following the
[DESIGN.md format](https://github.com/google-labs-code/design.md): YAML tokens
carry exact values, prose carries intent. The dashboard (internal/ui/dashboard.html)
must reference these tokens as CSS custom properties — no ad-hoc hex in components.

## Colors

Deep ink for headlines, slate for metadata, one accent for interaction.
Chart colors are a separate, validated palette (see Charts) — UI chrome never
borrows chart series hues, and status colors are never reused as accents.

```yaml
colors:
  light:
    surface:      "#fcfcfb"   # page & chart surface
    surfaceCard:  "#ffffff"
    border:       "#e7e6e1"
    inkPrimary:   "#0b0b0b"   # headlines, values
    inkSecondary: "#52514e"   # metadata, labels
    inkMuted:     "#8a8983"
    accent:       "#2a78d6"   # links, active tab, focus ring, primary button
    accentSoft:   "#cde2fb"
  dark:
    surface:      "#1a1a19"
    surfaceCard:  "#232322"
    border:       "#383835"
    inkPrimary:   "#ffffff"
    inkSecondary: "#c3c2b7"
    inkMuted:     "#8a8983"
    accent:       "#3987e5"
    accentSoft:   "#104281"
  status:          # fixed, never themed; always paired with icon or label
    good:     "#0ca30c"
    warning:  "#fab219"
    serious:  "#ec835a"
    critical: "#d03b3b"
```

## Typography

System stack (self-contained file, no webfonts). Space-Grotesk-like feel comes
from weight/letter-spacing, not from loading fonts.

```yaml
typography:
  body:     { fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif", fontSize: 14px, fontWeight: 400, lineHeight: 1.5 }
  label:    { fontSize: 11px, fontWeight: 600, letterSpacing: 0.06em, transform: uppercase, color: inkSecondary }
  heroValue:{ fontSize: 32px, fontWeight: 700, letterSpacing: -0.01em, color: inkPrimary }
  cardTitle:{ fontSize: 15px, fontWeight: 600 }
  mono:     { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", fontSize: 12.5px }  # paths, bytes
```

## Spacing & rounding

```yaml
spacing: { xs: 4px, sm: 8px, md: 16px, lg: 24px, xl: 32px }
rounding: { sm: 6px, md: 10px, lg: 14px, pill: 999px }
```

## Components

```yaml
components:
  card:      { backgroundColor: surfaceCard, border: border, rounded: lg, padding: lg, shadow: "0 1px 2px rgb(0 0 0 / 4%)" }
  statTile:  { extends: card, valueTypography: heroValue, labelTypography: label }
  badge:     { rounded: pill, padding: "2px 10px", typography: label }   # status badges use status colors at 12% bg + full-color text/icon
  tab:       { activeIndicator: "2px accent underline", inactiveColor: inkSecondary }
  buttonPrimary: { backgroundColor: accent, textColor: "#ffffff", rounded: md }
  buttonGhost:   { border: border, textColor: inkSecondary, rounded: md }
  table:     { headerTypography: label, rowBorder: border, rowHover: accentSoft at 25% }
```

## Charts

Palette = the dataviz reference instance (validated: adjacent CVD ΔE ≥ 8,
normal-vision ΔE ≥ 15, both modes). Rules: one axis; thin marks; 2px surface
gaps between fills; text wears ink tokens, never series color; single series
gets no legend; status colors only for state, with icon/label.

```yaml
charts:
  categorical:            # fixed order, never cycled; ≤4 series then fold to "Other"
    light: ["#2a78d6", "#008300", "#e87ba4", "#eda100"]
    dark:  ["#3987e5", "#008300", "#d55181", "#c98500"]
  sequentialBlue:         # single-series magnitude (directory/namespace bars)
    light: ["#cde2fb", "#86b6ef", "#3987e5", "#1c5cab"]
    dark:  ["#104281", "#1c5cab", "#3987e5", "#86b6ef"]
  gauge:                  # disk usage donut: colored by state thresholds
    ok: good, warn80: warning, warn90: serious, crit95: critical
    track: border
```

## OpenForge alignment

This project participates in the
[OpenForge OSS Design System](https://github.com/dasomel/openforge/blob/main/docs/design-system.md),
which standardizes semantics (state, focus, accessibility, foundational tokens)
without erasing product identity (accent, density, navigation, data-viz) — see
[ADR-0007](https://github.com/dasomel/openforge/blob/main/docs/adr/0007-design-system-standardizes-semantics-not-identity.md).
Nothing in this section changes a color, spacing, or component value defined
above; it only declares this project's archetype and maps existing tokens to
OpenForge's semantic roles.

### Archetype

`Operations Dashboard` (design-system.md §6): metrics-first, self-contained UI;
capacity and remaining values shown together; chart palette separated from
status; dense resource tables. This matches what Colors, Charts, and Voice
above already prescribe — remaining is always paired with used, chart hues stay
out of the fixed status set, and tables run dense rather than airy.

### Semantic token mapping (§3)

```yaml
openforgeMapping:
  color/bg/canvas:      surface
  color/bg/surface:     surfaceCard
  color/bg/subtle:      accentSoft        # nearest existing wash — see deviations
  color/bg/inverse:     (unmapped)        # no inverse surface exists — see deviations
  color/text/primary:   inkPrimary
  color/text/secondary: inkSecondary
  color/text/muted:     inkMuted
  color/text/inverse:   (unmapped)        # only a hardcoded #fff button label — see deviations
  color/border/default: border
  color/action/primary: accent
  color/action/hover:   accent            # + accentSoft as hover backdrop — see deviations
  color/focus/ring:     accent            # dashboard.html `--color-focus-ring`, new; aliases accent
  color/status/success: status.good
  color/status/warning: status.warning
  color/status/serious: status.serious
  color/status/danger:  status.critical
  color/status/info:    accent            # dashboard.html `--color-status-info`, new; aliases accent
```

### Intentional deviations (ADR-0007)

- **Accent stays project identity.** `accent` is this project's own blue, not a
  shared OpenForge hue — ADR-0007 reserves accent to the project by design.
- **`color/bg/subtle` has no clean analog.** This project has only two surface
  tiers (`surface`, `surfaceCard`), no third neutral "subtle" tier. Mapped to
  `accentSoft` as the closest existing wash, but that token is a hover/soft-accent
  color, not a neutral background — a real subtle-bg token would need to be
  introduced if this gap matters later.
- **`color/bg/inverse` / `color/text/inverse` left unmapped.** The only inverse
  value in the file is the hardcoded `#ffffff` label on `buttonPrimary`; there is
  no inverse surface anywhere in the UI. Left unmapped rather than invented.
- **`color/action/hover` is not one color.** Hover is conveyed by `accent`
  (border/text swap on ghost buttons and tabs) plus `--color-row-hover`, a
  translucent `accentSoft` wash — not a single distinct hover hue.
- **`color/status/info` and `color/focus/ring` are new tokens**, added to
  `dashboard.html` (`--color-status-info`, `--color-focus-ring`) to close this
  gap. Both alias `accent` rather than introduce a new hue, consistent with
  `accent`'s existing role above ("links, active tab, focus ring, primary
  button").
- **Density and data-viz stay exactly as documented** in Spacing & rounding and
  Charts above — ADR-0007 explicitly leaves density and chart-hue choices to
  the project.

## Voice

Numbers are the heroes: big value, small label, human units (`363.9 GiB`), raw
bytes only in tooltips. Remaining capacity is always shown next to used — never
make the reader subtract.
