## The Commandments of Clean Code

These are the sins you are looking for. Ordered by severity.

### I. Types
`any` is a plague. One untyped parameter infects every function it touches, silently disabling the compiler's protection. Use `unknown` + type guards, generics, and discriminated unions. If a value exists, it has a type. Prove it.

### II. Error handling
Never swallow errors silently. `catch (e) {}` means the error happened but no one will ever know. Every catch block logs with context. Every error speaks.

### III. Architecture
The domain depends on NOTHING. Domain entities do not import from infrastructure, UI, or external services. Layers have boundaries. Boundaries are enforced.

### IV. Tests
If it is not tested, it does not work. `expect(true).toBe(true)` is not a test — it is a suggestion. Assertions prove behavior. Coverage proves nothing without meaningful assertions.

### V. Secrets
No secret shall be committed. No hardcoded key, no inline credential, no .env checked in. If it touched git history, it is compromised. Rotate immediately.

### VI. Size
No file grows unchecked. 500+ lines is the warning. 1000+ is a creature. But split only when responsibilities are genuinely distinct — splitting for line count alone creates navigation overhead.

### VII. Dependencies
Every dependency must justify its existence. If it serves one function, write it yourself. Phantom dependencies (used but undeclared) collapse the build on the next lockfile change.

### VIII. Naming
Names are documentation. `data`, `temp`, `stuff`, `utils` — these are lies, not names. A file called `utils.ts` is a junk drawer. A variable called `val` is a secret. Names reveal intent.

### IX. Dead code
Dead code is a corpse. Commented-out blocks, unused functions, unreachable branches, zombie TODOs from 2023. Git remembers so you do not have to. Bury it.

### X. Git discipline
One commit, one logical change. The message is conventional, descriptive, and permanent. "update" is not a commit message — it is a sticky note.

## The Bestiary — creatures that live in undisciplined code

Recognize these patterns. They are what you are hunting.

- **The God Class** — A file that does everything: validation, persistence, formatting, logging. No one dares refactor it. Extract until each piece has one job.
- **The Silent Catch** — Swallows errors into the void. When production burns, the logs show nothing.
- **The Phantom Dependency** — Uses packages never declared. Survives only through hoisting. One lockfile change and everything collapses.
- **The console.log Ghost** — `console.log('here')`, `console.log('test')`, `console.log('????')`. Left behind by developers long gone, haunting production logs forever.
- **The Zombie TODO** — `// TODO: fix this later` — committed years ago. It will never be fixed. It will never be removed. If it is not tracked in an issue, it is not real.
- **The Infinite Re-render** — Born from an unstable object reference in a dependency array. The effect fires, creates a new object, which triggers the effect. The serpent eats its own tail.
- **The Syncing Store** — Copies query data into a store through a useEffect bridge, creating two sources of truth that slowly drift apart. Neither holds the truth. Both are lying.

## When NOT to act

Not every sin demands a crusade. These rules temper judgment:

- Three similar lines are better than a premature abstraction.
- A helper used once is just indirection.
- Splitting a 300-line file because it "might grow" is speculation, not refactoring.
- If nothing meaningful stands out, signal completion without changes. No-op is a valid outcome.
