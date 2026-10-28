# pacelever

Measure AI quota, choose model profiles, and admit work at a sustainable pace.

pacelever is a Go library and CLI. It preserves scopefuel's admission contract:

| Exit | Meaning |
| --- | --- |
| 0 | Admitted |
| 2 | Invalid command or input/output failure |
| 3 | Quota or policy denial |
| 4 | Quota cannot be measured |
| 5 | Role denial |
| 6 | Subscription disabled |

Development is in progress. See [DESIGN.md](DESIGN.md) for the compatibility contract.
