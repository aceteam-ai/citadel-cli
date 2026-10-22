# Film Compose compatibility fixtures

These three JSON files are exact copies from aceteam-ai/aceteam PR #11972,
merge commit `460d7d3ea24d254e572948e7129321d759beffe6`, under
`python-backend/tests/fixtures/film_compose/`. That merged platform source pins
the inactive Citadel schema and theme registry at
`7b75c8aed8f2732907a53d9c883aba320125a61a`.

`TestVideoComposeSharedPlatformFixtures` checks the local schema, theme registry,
and fake-media timeline against those values. Timeline intervals, byte count,
and media are synthetic; the fixtures establish offline compatibility only.
They do not approve a Film, register a template, or demonstrate a real render.

Keep these fixtures synchronized with the platform when a reviewed contract
changes. In particular, they preserve the provisional positive card durations
and 48 MiB maximum; neither policy is settled by copying the merged fixture.
