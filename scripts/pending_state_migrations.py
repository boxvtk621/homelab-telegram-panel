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

# R11 adds the immutable normalized history ledger used by the R12 PostgreSQL
# replica. The binary can migrate a stopped local database from v4 to v5, but
# ordinary component deployment must still reject this state transition until
# the later reviewed rollout stage supplies the all-node operational gate.
R11 = {
    'id': 'harness-schema-v4-to-v5',
    'issue': 'HL-294@1',
    'compatibility': {
        'cursor': {
            'from': 'd87c1f13f79b80eb2195619bfdb55f4a26a86f01596f86ba6e860c8e1573f928',
            'to': '776ce3ab889f473ab44427699d8d7d9e4b9604e45fb047bedc21b38bfa4ea3a3',
        },
        'codex': {
            'from': '1704d5fa6de833b47fd9562a95ec2f4733ebf742867a06d403d2814121d15338',
            'to': '2816e7f3783ca5f0d2459ed8d64e1993c50430708349bbf0e1e1bcb1348e19e0',
        },
    },
}

# R13 adds the durable logical-delete receipt used to keep archived history
# hidden without physically purging retained data. The binary can migrate a
# stopped local database from v5 to v6, but ordinary component deployment must
# keep rejecting the transition until a reviewed rollout performs it.
R13 = {
    'id': 'harness-schema-v5-to-v6',
    'issue': 'HL-295@1',
    'compatibility': {
        'cursor': {
            'from': '776ce3ab889f473ab44427699d8d7d9e4b9604e45fb047bedc21b38bfa4ea3a3',
            'to': '6ec13da63d26103396015f00a21a3941e268c5d7152170233fe46b942f19d1eb',
        },
        'codex': {
            'from': '2816e7f3783ca5f0d2459ed8d64e1993c50430708349bbf0e1e1bcb1348e19e0',
            'to': 'b1ceb672fb365ccba10943dd3ccfe2cb7ae2a3ea82efc96bfc369f7daa56f3be',
        },
    },
}
