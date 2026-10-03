# AGENTS.md

How to work in this project, and how to write its documentation. These rules apply
to every repository in the platform: `town`, `pestilence`, `scarab`, `my-opps`, and
`helm-charts`.

## Ask before you push

A local change is easy to undo. A push is not. It is public, it starts CI, and a tag
publishes an image or a chart. So:

- Do not push unless the task says to push or publish. "Prepare the release" and
  "cut v1.0.1" are not permission to push.
- Keep editing, committing, and tagging separate. Do not combine them in one command.
- Do not create or push a tag without an explicit instruction.
- Stop before a push. Report the branch, and the tag if there is one. Then wait.
- A commit is not a push. Commit to keep a change set coherent, say that you did, and
  leave the push to the operator.
- Deleting is the same category: a remote branch, a tag, a published package, or a
  cluster object.
- A cluster change needs a task that asks for it. Say what you will run before you run
  it.

## Two registers, and no mixing

| Document | Reader | Style |
|---|---|---|
| `README.md` | Someone new to the project | Terse. Plain language. |
| `docs/*.md` | An engineer who must build on it | Simplified Technical English. |
| Code comments | An engineer reading the code | Plain language. Explain the decision, not the code. |

A README introduces the project and links to the docs. The docs state the design
and the contracts. Do not put design reasoning in a README. Do not put a
beginner's introduction in a doc.

## What belongs in the repository

Write contracts and designs. Nothing else.

- Keep: interfaces, values, ports, paths, labels, names, limits, invariants,
  procedures, and the reasons a contract has its shape.
- Leave out: review history, defect logs, war stories, self-critique, and
  personal reasoning. Move that to the private files outside the repository.
- Leave out: private hostnames, node names, IP addresses, e-mail addresses,
  registrars, and account names. Use a placeholder such as `example.com` or
  `node-1`.
- Leave out: secrets. Never commit a key, a token, or a password.

## Rules for both registers

1. Write for a reader who does not know the project.
2. Use one term for one thing. Do not use two words for the same concept.
3. Use the active voice and the present tense.
4. Keep sentences short. Aim for fewer than 20 words.
5. Put one idea in each sentence. Put one topic in each paragraph.
6. Use a list for three or more items. Use a table for values or comparisons.
7. Mark code, paths, variables, and literal values with backticks.
8. Give exact values. Write `120 seconds`, not `a short time`.
9. Delete a sentence that adds no fact and no instruction.
10. Write repository names in lower case. Capitalise them only at the start of a
    sentence.
11. Use British spelling: `organisation`, `authorise`, `behaviour`.
12. Use sentence case in headings: `## How it fits together`, not `## How It Fits
    Together`.
13. Use a relative link inside a repository. Use an absolute GitHub URL across
    repositories.

## Rules for README.md

The README answers four questions, in this order:

1. What is this?
2. How does it fit with the other repositories?
3. How do I build and run it?
4. Is it finished?

Use this structure:

    # <name>

    One sentence: what it is.

    One or two sentences: what it does for a user.

    ## How it fits together

    - **<repo>** <one line>.
    - **<repo>** <one line>.
    - **<repo>** <one line>.

    One sentence: what talks to what.

    ## Development

    ```sh
    <the exact commands>
    ```

    ## Project status

    One or two sentences: what works, what is missing.

    See [<topic>](docs/<file>.md) for <what the doc adds>.

- Keep the README under 60 lines.
- Do not name implementation libraries in the first paragraph. A reader cares
  what the project does, not which framework it uses.
- Do not copy text from the docs. Link to the docs.
- Do not add badges, screenshots, or feature tables.
- End each list item with a period.

## Rules for docs/*.md

Base the language on ASD-STE100 Simplified Technical English. Apply these rules:

- Write one instruction in each sentence.
- Keep an instruction under 20 words. Keep a description under 25 words.
- Write the subject before the verb. Write the verb before the object.
- Use a verb for an action. Write `check the file`, not `perform a check of the
  file`.
- Do not use an `-ing` verb form for an action. Write `start the broker`, not
  `starting the broker`.
- Do not write a noun group longer than three words.
- Define each term at the first use. Put an abbreviation in brackets after the
  full term.
- Do not use an idiom, a metaphor, humour, or slang.
- Do not use a dash to join two independent clauses. Write two sentences.
- Use `must` for a requirement, `can` for a possibility, and `do not` for a
  prohibition.

## Words to choose

| Do not write | Write |
|---|---|
| utilise, leverage | use |
| in order to | to |
| prior to | before |
| commence | start |
| terminate | stop |
| facilitate | help |
| approximately | about |
| sufficient | enough |
| additional | more |
| ensure | make sure, check |
| is able to, has the ability to | can |
| a number of | several |
| in the event that | if |
| at this point in time | now |

Delete these words: `simply`, `just`, `obviously`, `clearly`, `basically`,
`actually`, `really`, `very`, `note that`, `please`, `it should be noted`.

## Worked examples

**A README first line.** Remove implementation names. Name the user benefit.

Before:

> The management UI for the Pi agent platform: a Hono BFF and a React front end,
> in one deployment.

After:

> The management interface for a self-hosted platform that gives people temporary
> workspaces for Pi coding agents.

**An architectural sentence.** Split it. Remove the aside. State the rule first.

Before:

> The obvious fix — widening the session cookie to `Domain=.gobackto.work` — is
> worse than it looks. Tenant workspaces are sibling subdomains serving
> tenant-controlled content, so widening hands every tenant a cookie they can
> shadow, and one tenant can then log out every other tenant.

After:

> Do not widen the session cookie to `Domain=.gobackto.work`. Tenant workspaces
> are sibling subdomains. They serve tenant-controlled content. A widened cookie
> lets one tenant shadow the cookie of another tenant. One tenant can then sign
> out every other tenant.

## Checklist before you commit

1. Read the text aloud. Split every sentence that runs out of breath.
2. Replace every word from the two tables above.
3. Delete every sentence that does not add a fact or an instruction.
4. Check that one term is used for one thing.
5. Check that no private hostname, address, account, or key is present.
6. Check that the README links to the docs instead of repeating them.
