"""Static contract tests for the self-contained NPM bootstrap script."""
from pathlib import Path
import re
import unittest


SOURCE = Path(__file__).with_name('configure-panel-npm.mjs').read_text()
MANAGED = re.search(r"const addition = `(?P<body>.*?)`;", SOURCE, re.DOTALL).group('body')


def locations(config):
    """Return literal nginx location selectors and their balanced bodies."""
    result = {}
    for match in re.finditer(r'^location\s+([^\{]+?)\s*\{', config, re.MULTILINE):
        depth = 1
        cursor = match.end()
        while depth and cursor < len(config):
            if config[cursor] == '{':
                depth += 1
            elif config[cursor] == '}':
                depth -= 1
            cursor += 1
        if depth == 0:
            result[match.group(1).strip()] = config[match.end():cursor - 1]
    return result


class PanelNPMContractTest(unittest.TestCase):
    def test_only_exact_readback_routes_disable_basic_auth(self):
        blocks = locations(MANAGED)
        expected = {
            '= /panel/components.json',
            '= /panel/deployment-status.json',
            '= /panel/api/v2/healthz',
        }
        public = {selector for selector, body in blocks.items() if 'auth_basic off;' in body}
        self.assertEqual(public, expected)
        for selector in expected:
            with self.subTest(selector=selector):
                body = blocks[selector]
                self.assertIn('limit_except GET { deny all; }', body)
                self.assertIn('proxy_pass https://10.202.2.52:18443;', body)
                self.assertIn('proxy_method GET;', body)
                self.assertIn('proxy_ssl_verify on;', body)
                self.assertIn('proxy_set_header X-Panel-Authenticated-User "";', body)
                self.assertNotIn('auth_basic_user_file', body)

    def test_panel_catch_all_remains_authenticated(self):
        body = locations(MANAGED)['^~ /panel/']
        self.assertIn('auth_basic "HomeLab Agent Panel";', body)
        self.assertIn('auth_basic_user_file ${access};', body)
        self.assertIn('proxy_set_header X-Panel-Authenticated-User $remote_user;', body)
        self.assertNotIn('auth_basic off;', body)

    def test_no_other_public_panel_content_route_exists(self):
        blocks = locations(MANAGED)
        self.assertEqual(set(blocks), {
            '= /panel',
            '= /panel/components.json',
            '= /panel/deployment-status.json',
            '= /panel/api/v2/healthz',
            '^~ /panel/',
        })
        self.assertEqual(blocks['= /panel'].strip(),
                         'return 308 https://h1-cloud.ru/panel/;')


if __name__ == '__main__':
    unittest.main()
