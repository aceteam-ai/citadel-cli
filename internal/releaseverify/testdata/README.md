These are inert offline protocol fixtures, not release assets. The archive
fixture is plain text and is never extracted or executed. The manifest and its
signature are reproducible from RFC 8032 section 7.1's first public test seed.
The fixture key is never installed as a production trust root. Tests also use a
second deterministic test-only seed for rotation overlap and retirement.
