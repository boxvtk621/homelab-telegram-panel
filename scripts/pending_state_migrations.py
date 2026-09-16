"""Pinned incompatible state candidates that have no deploy operation yet."""

# R05 changes the durable Harness database identity. Publication may carry the
# new compatibility hashes, but the ordinary deploy path must keep rejecting
# the transition until a reviewed operator migration is supplied.
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

# R06 extends that database identity with proof revisions and durable hold
# outcomes. It remains a separate incompatible candidate: later deployment
# stages own the process effect and operator migration across either boundary.
R06 = {
    'id': 'harness-schema-v3-to-v4',
    'issue': 'HL-289@1',
    'compatibility': {
        'cursor': {
            'from': '1de4b9d847cb818c02958acccf52027639947763fcc6517c42db81f2ff8d1c39',
            'to': 'd87c1f13f79b80eb2195619bfdb55f4a26a86f01596f86ba6e860c8e1573f928',
        },
        'codex': {
            'from': '27ee16930649651ec9cd5ac88eb2c9a916437e49f3d329acf0c1b4570708636b',
            'to': '1704d5fa6de833b47fd9562a95ec2f4733ebf742867a06d403d2814121d15338',
        },
    },
}
