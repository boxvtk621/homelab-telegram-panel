"""Pinned incompatible state candidates that have no deploy operation yet."""

# R05 changes the durable Harness database identity. Publication may carry the
# new compatibility hashes, but the ordinary deploy path must keep rejecting
# the transition until R06 supplies a reviewed operator migration.
R05 = {
    'id': 'harness-schema-v2-to-v3',
    'issue': 'HL-288@1',
    'compatibility': {
        'cursor': {
            'from': '6f9a76ab4b6a6594e59d791c132b679db59952258300adc5b66bebd5bf78ebfd',
            'to': '1de4b9d847cb818c02958acccf52027639947763fcc6517c42db81f2ff8d1c39',
        },
        'codex': {
            'from': '7b401e44b516a78dbaee8b25ba8c58943a382ce72233622c60cb8167dea18096',
            'to': '27ee16930649651ec9cd5ac88eb2c9a916437e49f3d329acf0c1b4570708636b',
        },
    },
}
