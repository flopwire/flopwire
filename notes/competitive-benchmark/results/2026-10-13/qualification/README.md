# Earlier qualification records

These records qualified the runners before the final current-main run.
Flopwire was built from the initial local checkout at
`8023e172ecbd7e794e732a3c57b62b4c4b994797`. That checkout was behind
`origin/main`. The dated report's primary tables use the later complete
rerun against main `f83db684e912ea77c08ac02264996f55cf1f4993`.

The earlier retrieval run measured the three products together and `rg`
separately. All applicable exact queries passed. Its extra recovery send
after 30 messages hit Flopwire's default hourly session allowance. That
failure remains in `messaging.json.gz`. The subsequent isolated recovery
run passed for both products.

The raw records retain the earlier measurements and their limits. They
are not a controlled A/B comparison with the current-main records.
