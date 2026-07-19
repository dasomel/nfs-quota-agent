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

## Voice

Numbers are the heroes: big value, small label, human units (`363.9 GiB`), raw
bytes only in tooltips. Remaining capacity is always shown next to used — never
make the reader subtract.
