# Learnings — Week 1 (Coordinate Generator)

Concepts and interview-ready takeaways from building the reproducible data generator.
Report: [week1.md](week1.md)

## 1. Reproducibility via seeded RNG
A pseudo-random generator (`std::mt19937`, the Mersenne Twister) is **deterministic**: same seed →
same sequence, forever. That's not a limitation, it's a feature — it makes bugs reproducible and
benchmarks comparable run-to-run.
- **Key move:** if the user doesn't supply a seed, generate one randomly *and print it*, so a
  "random" run is still reproducible after the fact.
- **Interview angle:** *"How do you make a randomized test reproducible?"* → seed the RNG, log the
  seed, never call the un-seeded default. Also: `std::random_device` for the seed, `mt19937` for
  the stream — `random_device` can be slow/non-reproducible, so you use it once to seed, not per draw.

## 2. stdout vs stderr — the Unix data/metadata split
Data goes to **stdout**; diagnostics (seed, parameters, progress) go to **stderr**. This is why
`./generator | ./consumer` works — the pipe carries only clean data. Mixing logs into stdout
corrupts the stream.
- **Interview angle:** *"Why print logs to stderr?"* → so stdout stays a pure, pipeable data
  channel; the two streams are independently redirectable (`2>/dev/null`, `1>data.csv`).

## 3. Designing a CLI properly
- Everything configurable is a **flag with a default**, never a hard-coded constant (you should
  never recompile to change dataset size).
- Validate inputs (`counts >= 0`, `grid > 0`, known format) and exit with a **non-zero code** on
  bad input. Exit codes matter for scripting.
- **Interview angle:** relates to the "config over constants" principle — 12-factor-app thinking,
  even for a CLI tool.

## 4. Machine-readable output formats
Supporting text/csv/json means the data is consumable by humans *and* programs. The subtle
requirement: **the same seed must produce the same coordinates across all formats.** That only
holds if the number and order of RNG draws is identical regardless of format (here: exactly 2
draws per entity, riders before drivers). Format is a *presentation* concern layered on a fixed
*data* sequence.

## 5. Single Responsibility Principle in practice
The interim matcher was deliberately kept OUT of the generator. A generator generates; it doesn't
match. Small, single-purpose tools compose better and are easier to test.
- **Interview angle:** SRP isn't just OOP class design — it applies to whole programs/modules.

## Quick self-test
- Why seed an RNG and still print the seed on a "random" run?
- What breaks if you print progress logs to stdout instead of stderr?
- Why must RNG draw *order* be fixed for cross-format reproducibility?
