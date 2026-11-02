# AGENTS.md

How to work in this project, and how to write its documentation. These rules apply
to every repository in the platform: `town`, `pestilence`, `scarab`, `my-opps`, and
`helm-charts`.

## Git discipline

Do not push unless the task says to push or publish. "Prepare the release" is not
permission to push.

- Stop before a push. Report the branch, and the tag if there is one. Then wait.
- Do not create or push a tag without an explicit instruction.
- A commit is not a push. Commit to keep a change set coherent, and say that you did.
- Do not delete a remote branch, a tag, a published package, or a cluster object
  without an explicit instruction.
- Do not change a cluster unless the task asks for it. Say what you will run before
  you run it.

## Two registers

| Document | Reader | Style |
|---|---|---|
| `README.md` | Someone new to the project | Terse, plain language |
| `docs/*.md` | An engineer who must build on it | Simplified Technical English |
| Code comments | An engineer reading the code | Explain the decision, not the code |

The README introduces the project and links to the docs. The docs state the design
and the contracts. Do not put design reasoning in a README, or an introduction in a
doc.

## What belongs in the repository

Keep contracts and designs: interfaces, values, ports, paths, labels, limits,
procedures, and the reasons a contract has its shape. Leave out review history,
defect logs, war stories, and personal reasoning. Use a placeholder such as
`example.com` or `node-1` for private hostnames, addresses, and accounts. Never
commit a key, a token, or a password.

## Style rules

- Write for a reader who does not know the project.
- Use the active voice and the present tense. Keep sentences under 20 words, with
  one idea each.
- Use one term for one thing. Define a term, and its abbreviation, at first use.
- Use a verb for an action: `start the broker`, not `perform a check`. Do not use an
  `-ing` form for an action.
- Use a list for three or more items. Use a table for values or comparisons.
- Mark code, paths, and literal values with backticks. Give exact values.
- Do not use an idiom, a metaphor, humour, or slang. Do not join two clauses with a
  dash. Write two sentences.
- Use `must` for a requirement, `can` for a possibility, and `do not` for a
  prohibition.
- Use British spelling. Use sentence case in headings.
- Use a relative link inside a repository, and an absolute GitHub URL across
  repositories.
- Write repository names in lower case.
- Delete every sentence that adds no fact and no instruction.

## Words to choose

| Do not write | Write |
|---|---|
| utilise, leverage | use |
| in order to | to |
| prior to | before |
| commence | start |
| terminate | stop |
| facilitate | help |
| ensure | make sure, check |
| approximately | about |

Delete these words: `simply`, `just`, `obviously`, `clearly`, `basically`,
`actually`, `really`, `very`, `note that`, `please`.

## README structure

The README answers four questions, in this order: what is this, how does it fit with
the other repositories, how do I build and run it, and is it finished.

    # <name>

    One sentence: what it is.

    One or two sentences: what it does for a user.

    ## How it fits together

    - **<repo>** <one line>.

    One sentence: what talks to what.

    ## Development

    ```sh
    <the exact commands>
    ```

    ## Project status

    One or two sentences: what works, what is missing.

    See [<topic>](docs/<file>.md) for <what the doc adds>.

- Keep the README under 60 lines, and each list item ended with a period.
- Do not name an implementation library in the first paragraph.
- Do not copy text from the docs. Link to them.
- Do not add badges, screenshots, or feature tables.

## Rules for docs/*.md

- Write one instruction in each sentence. Keep it under 20 words, and a description
  under 25.
- Write the subject before the verb, and the verb before the object.
- Do not write a noun group longer than three words.
