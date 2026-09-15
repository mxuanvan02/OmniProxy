# Phase 05 — Wire the section into navigation

**Priority:** Critical — without this the page exists but is unreachable.
**Status:** Not started
**Depends on:** phase 04
**Blocks:** phase 06

## Context links

- `plans/20260915-1622-providers-view/design.md` §5 (registration table)
- `web-next/src/components/Shell.tsx`, `web-next/src/App.tsx`

## Overview

Six one-line registration edits across two files, plus the first test this repo
has ever had for the navigation shell.

## Key insights

**1. One array feeds both navigations.** `Shell.tsx:4-12`'s `items` is mapped by
the desktop sidebar (`:18`) *and* by the mobile `<select>` (`:23`). Adding one
entry covers both; adding it in only one place is impossible.

**2. `VALID_SECTIONS` is not a type-level guard.** `App.tsx:29-32`'s
`initialSection()` does `VALID_SECTIONS.includes(value) ? value : 'overview'`,
so a section missing from that array silently falls back to Tổng quan while the
nav button still appears — clicking it changes the hash and nothing else. That
is the failure mode this phase's test does not cover and the `tsc -b` step does
not either, so it is worth a comment in the source.

**3. Both `usagePeriod` and `pool` are needed, and `usagePeriod` is shared.**
The providers page binds the same `usagePeriod` state the Usage view uses, so
switching period on one and navigating to the other keeps the selection. That is
intended.

**4. `api.poolHealth()` failing takes the whole `load()` down.** `load()` has one
try/catch and one error banner, so a 404 on the new route would blank the page
rather than degrade. The route ships in the same binary as the bundle (phase 02
and phase 06 are one release), so this cannot happen in practice — but do not
"fix" it by swallowing the error, which would hide a genuinely mis-built binary.

## Requirements

**Functional**
- `#providers` resolves to the providers section on a fresh load.
- The nav button appears on desktop and the option appears in the mobile select.
- Selecting the section loads accounts (already unconditional via `loadCore`),
  usage for the current period, and the pool health snapshot.
- Switching the period on this page refetches usage for the new period.

**Non-functional**
- The 15s auto-refresh stays limited to `accounts` and `overview`.
- No change to `AccountsView.tsx`.

## Architecture

```
Shell nav click ──► onSection('providers') ──► App.setSection ──► history #providers
                                                      │
                                                      ▼
                                       load() ──► loadCore()            (accounts, status, capabilities)
                                              ├─► api.usage(usagePeriod)
                                              └─► api.poolHealth()
                                                      │
                                                      ▼
                              <ProvidersView accounts usage pool period onPeriod onReload />
```

## Related code files

- **Modify:** `web-next/src/components/Shell.tsx` — `Section` union (`:3`), `items` (`:4-12`)
- **Modify:** `web-next/src/App.tsx` — `VALID_SECTIONS` (`:25`), `pool` state, `load()` (`:77-102`), content ternary (`:135-147`)
- **Create:** `web-next/src/components/Shell.test.tsx`
- **Delete:** none

## Implementation steps

### Step 1: Write the failing test

Create `web-next/src/components/Shell.test.tsx`:

```tsx
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Shell } from './Shell'

function renderShell(onSection = vi.fn()) {
  render(<Shell section="overview" onSection={onSection} onRefresh={vi.fn()} onLogout={vi.fn()} busy={false} updatedAt={null}>nội dung</Shell>)
  return onSection
}

describe('Shell navigation', () => {
  it('lists the providers section and reports it when clicked', () => {
    const onSection = renderShell()

    fireEvent.click(screen.getByRole('button', { name: /Nhà cung cấp/ }))
    expect(onSection).toHaveBeenCalledWith('providers')
  })

  // The desktop sidebar and the mobile <select> both map the same array, so a
  // missing entry would drop the page from one of them.
  it('offers the section in the mobile selector as well', () => {
    renderShell()
    expect(screen.getByRole('option', { name: 'Nhà cung cấp' })).toBeTruthy()
  })

  it('keeps the existing sections', () => {
    renderShell()
    for (const label of ['Tổng quan', 'Tài khoản', 'Sử dụng', 'Hạn mức', 'API & CLI', 'Thiết lập', 'Nhật ký']) {
      expect(screen.getByRole('option', { name: label })).toBeTruthy()
    }
  })
})
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- Shell.test.tsx 2>&1 | tail -20
```

Expected: 2 fail (`Unable to find role="button" with name /Nhà cung cấp/`, and the
option is missing); the third passes.

### Step 3: Register the section

In `web-next/src/components/Shell.tsx`:

**3a.** Replace line 3:

```ts
export type Section = 'overview'|'accounts'|'providers'|'usage'|'quota'|'api'|'settings'|'logs'
```

**3b.** In `items`, insert after the `accounts` entry (`:6`):

```ts
  {id:'providers',label:'Nhà cung cấp',hint:'Vendor, model & lỗi',icon:'⇄'},
```

### Step 4: Register the route

In `web-next/src/App.tsx`:

**4a.** Replace line 25:

```ts
const VALID_SECTIONS: Section[] = ['overview', 'accounts', 'providers', 'usage', 'quota', 'api', 'settings', 'logs']
```

**4b.** Add the import next to the other components (after the `Overview` import on line 6):

```ts
import { ProvidersView } from './components/ProvidersView'
```

**4c.** Add the type import — the list on lines 10-23 is alphabetical, so
`PoolHealth` goes after `ChartPoint`:

```ts
  type PoolHealth,
```

**4d.** Add the state, after `usagePeriod` (`:50`):

```ts
  const [pool, setPool] = useState<PoolHealth | null>(null)
```

**4e.** In `load()`, after the `overview` branch (`:82`):

```ts
      if (section === 'providers') {
        const [nextUsage, nextPool] = await Promise.all([api.usage(usagePeriod), api.poolHealth()])
        setUsage(nextUsage)
        setPool(nextPool)
      }
```

**4f.** In the content ternary, insert one arm after the `accounts` arm (which
ends on `:138`) and before the `usage` arm (`:139`):

```tsx
      : section === 'providers'
        ? <ProvidersView accounts={accounts} usage={usage} pool={pool} period={usagePeriod} onPeriod={setUsagePeriod} onReload={load} />
```

The resulting chain reads `overview → accounts → providers → usage → quota → api
→ settings → logs`, matching the nav order.

### Step 5: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -25
```

Expected: all suites pass, including the three new Shell tests and phase 04's
ten.

### Step 6: Typecheck and lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npx tsc -b 2>&1 | tail -20
npm run lint 2>&1 | tail -15
```

Expected: no output from `tsc -b`. This is the step that proves the ternary arm
and `ProvidersView`'s prop types agree — a missing or misspelled prop fails here.

### Step 7: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/Shell.tsx web-next/src/components/Shell.test.tsx web-next/src/App.tsx
git commit -F - <<'EOF'
feat(web-next): add the providers section to the admin navigation

One entry in Shell's items array covers both the desktop sidebar and the
mobile select, since both map the same list. App adds the section to
VALID_SECTIONS, loads usage for the current period plus the pool health
snapshot when it is selected, and renders the view.

It stays out of the 15s auto-refresh: that effect is limited to accounts and
overview, and /usage/stats is the largest polled payload at roughly 205 KiB.
The refresh path is the manual button.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `Shell.test.tsx` with 3 tests
- [ ] Step 2: confirm 2 of them fail
- [ ] Step 3: `Section` union + `items` entry in `Shell.tsx`
- [ ] Step 4: `VALID_SECTIONS`, import, `pool` state, `load()` branch, ternary arm in `App.tsx`
- [ ] Step 5: full suite green
- [ ] Step 6: `tsc -b` and `npm run lint` clean
- [ ] Step 7: commit

## Success criteria

- `Shell.test.tsx` passes; the nav entry reaches both the sidebar and the select.
- `npx tsc -b` clean — proof that the ternary arm's props match `ProvidersView`.
- Loading `/admin-next/#providers` directly lands on the page, not on Tổng quan.
- The 15s interval still does not fire for this section.

## Risk assessment

| Risk | Mitigation |
|---|---|
| `'providers'` added to `items` but not `VALID_SECTIONS` → deep link silently falls back to Tổng quan | Step 4a; the manual deep-link check in success criteria |
| Ternary arm placed in the wrong position and swallowing a later section | Step 4f names the exact neighbours; `tsc -b` plus the existing section tests |
| `usagePeriod` shared with the Usage view and surprising | Intended; documented in key insight 3 |
| `api.poolHealth()` 404 blanking the page | Route and bundle ship in one binary; deliberately not masked |

## Security considerations

- **No new authorization surface.** The new route sits behind the same
  `adminSessions.valid()` gate at `proxy/handler.go:6151` as every other
  `/admin/api/` route.
- **No new state persisted.** `pool` is component state, cleared on reload.
- **The section adds no mutating action**, so a mis-registration cannot cause a
  write.

## Next steps

Phase 06 builds `web-next` into `webnext/dist`, which `webnext/embed.go` embeds
with `//go:embed all:dist`, then rebuilds and restarts the Go binary so the new
route and the new bundle ship together.
