# art-source

Source material for the illustration at the top of the main branch's README (`docs/assets/attic-cloud.svg`).
This branch is deliberately separate from `main`: it shares no history with it, and nothing on `main` depends on it.

* `build.py` builds `attic-cloud.svg` from hand-written SVG plus the logos, embedding them as data URIs
  (`python3 build.py`, standard library only). Output is byte-identical to the file on `main`.
* `logos/` holds the logo files that were embedded.

The AWS, Google Cloud, OpenAI and Anthropic logos are trademarks and artwork of their owners. They are used here
only as a joke about big clouds and imply no affiliation or endorsement. Check each company's brand guidelines before
reusing them anywhere else.
