# Contributing to kubexa-agent

Patches from clusters we cannot reproduce are the most useful kind: managed
distributions, arm64 node pools, restrictive RBAC, CRD-heavy installs. The
conventions this tree holds to — where a comment belongs, what ships disabled
by default, what counts as a test, how the chart schema is part of the contract
— are in the README's [Contributing](README.md#contributing) section. Read that
before a first patch. This file is only about the terms.

## Terms

By opening a pull request against this repository you confirm that:

1. **You wrote the contribution, or you have the right to submit it.** It is
   your own work, or you submit it with the permission of whoever owns it, and
   you are not knowingly submitting someone else's code under a licence that
   does not permit it.

2. **You grant the copyright holder of this repository a perpetual, worldwide,
   non-exclusive, royalty-free, irrevocable licence to use your contribution
   and to sublicense it, including under licences other than GPL-3.0.** Your
   contribution stays available to everyone else under GPL-3.0, exactly like
   the rest of this repository. You keep your copyright; you are granting a
   licence, not assigning ownership.

## Why the second term exists

We would rather explain this than have it read as boilerplate.

The agent is GPL-3.0 because it runs in your cluster holding credentials to it,
and a component like that should be readable by the person who has to sign off
on it. The rest of Kubexa is proprietary, and it shares two things with the
agent: the wire contract under [`proto/`](proto/) and the agent configuration
schema. Both are compiled into the Kubexa server images.

That is sound today because the copyright in this repository is undivided —
GPL-3.0 is a licence *to recipients*, and a copyright holder needs no licence
from itself. A contribution merged without term 2 would divide it: that
contributor's code would sit inside a proprietary binary under a licence that
does not permit it being there, and the only remedies left would be ripping the
contribution back out or opening the server components. Neither is a decision
anyone should discover after the fact, so we ask once, up front.

If you would rather not grant that licence, say so in the pull request. It is
not a dead end: a clear bug report is genuinely useful on its own, and many
fixes can be reimplemented from a description of the problem rather than from
the patch.
