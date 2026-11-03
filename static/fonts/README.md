Terminal fonts for `pip.html`, so phones (which have no Menlo or Nerd Font)
draw prompt icons and Claude Code's symbols instead of empty boxes.

| File | Source | Licence |
| --- | --- | --- |
| `cw-mono.woff2` | Hack Nerd Font Mono Regular: text, box drawing | Hack: MIT + Bitstream Vera |
| `cw-mono-icons.woff2` | Hack Nerd Font Mono: powerline, Font Awesome, devicons, octicons, codicons | Nerd Fonts: MIT; icon sets: their own (OFL, MIT) |
| `cw-mono-material.woff2` | Hack Nerd Font Mono: Material Design icons | Apache 2.0 |
| `cw-symbols.woff2` | JuliaMono Regular: ⎿ ⏺ ⏵ ✻, checks, braille, math | SIL OFL 1.1, see `LICENSE-JuliaMono.txt` |

All four are subsets and renamed (CW Mono, CW Symbols), as both licences ask
of modified fonts. Each is loaded by `unicode-range`, so a page downloads only
the files its text needs. The server caches `/fonts/` for a year: give a
changed font a new file name.
