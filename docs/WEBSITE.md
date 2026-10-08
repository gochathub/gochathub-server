# Website — gochathub.com (agent brief)

Marketing site for the GoChatHub product. Lives in its **own repository**, not this
server repo. This brief records the requirements discovered 2026-10-08; the server
repo stays single-purpose.

## Decisions (settled)

- **Product model**: self-hosted open-source project **plus** hosted service.
  Pricing section stays, hosted tiers on top of a free/self-hosted tier.
- **Stack**: Hugo static site generator, **custom theme**. The Colorlib "saas"
  template (preview.colorlib.com/theme/saas/) is the *visual reference only* —
  its 2018 Bootstrap/jQuery code ships nowhere, so no Colorlib attribution/licensing
  applies (their free license requires the footer credit only if their code is used).
- **Location**: separate repo (e.g. github.com/gochathub/gochathub-web or the
  site repo), deployed as static output (CF Pages, nginx, or similar).

## Visual language (borrowed from the reference template)

- Palette: azure/blue primary, deep navy footer, light body. Light-only unless
  the dark-mode question (below) is answered.
- Rhythm: centered hero → 3-icon feature trio → product showcase panel →
  numbered steps → CTA band → pricing cards → footer. Mobile stacks single-column
  (verified in the template at 390px; keep that behavior).
- Typography scale similar to the reference; real fonts from Google Fonts.

## Sections (template map → gochathub)

1. **Hero** — headline for "self-hosted chat you own", real web-client screenshot
   (template's laptop mockup is placeholder), CTAs to install and hosted signup.
2. **Feature trio** — three icons: Go chat server, Android client (UnifiedPush),
   web client.
3. **Openness/security panel** — replaces the template's chart mockup: Go, PostgreSQL,
   REST/OpenAPI, self-host everything, message data stays on your infrastructure.
4. **3 steps** — install, connect, invite (template's 6-step process collapses).
5. **CTA band** — "Run it on your own server".
6. **Demo video** — only if one exists (unverified; cut if not).
7. **Pricing** — free self-hosted + paid hosted tiers (tier names/prices: unknown).
8. **Docs/support** — links to docs, API contract, GitHub, server status.
9. **Blog** — Hugo content engine (MD/MDX in content/); real posts only.
10. **Footer** — repo links, docs, API, status, legal/hosted TOS. Real favicon
    (the template's own `img/fav.png` 404s — do not repeat).

## Assets needed

- Logo pack: `../gochathub-logo-pack` (brand assets).
- Real screenshot(s) of the gochathub web client.
- Pricing tier definitions (names, monthly prices, limits).

## Non-functional requirements

- No JS dependencies beyond minimal vanilla (template ships jQuery/plugin stack —
  drop all of it).
- Mobile-first, no horizontal scroll at 390px.
- No third-party analytics by default; if wanted, privacy-respecting only.
- Fast static hosting, no server-side rendering required.
- Accessible: alt text, contrast on the navy/blue palette, keyboard nav.

## Content rule

Target audience is developer self-hosters: no marketing filler ("Stunning
Visuals"-style copy fails with this audience). Testimonials appear only if real.

## Out of scope

- JS frameworks/SPA, CMS, Colorlib template code, cookies/analytics banners
  (no analytics ⇒ no banner), i18n beyond English (unasked).

## Open questions

1. Hosted tier names, prices, and limits.
2. Demo video exists or not.
3. Dark mode (template is light-only; web client may be dark-first).
4. Analytics preference (none / self-hosted privacy-respecting).
5. Hosting target for the static output (CF Pages / own nginx / other).

Next step: `/sc:design` for the site architecture, then scaffold the repo.