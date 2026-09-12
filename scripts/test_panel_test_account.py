"""Static safety contract for the scoped Panel test-account provisioner."""
from pathlib import Path
import unittest


SOURCE = Path(__file__).with_name('provision-panel-test-account.mjs').read_text()


class PanelTestAccountContractTest(unittest.TestCase):
    def test_uses_a_panel_only_access_file(self):
        self.assertIn("const sharedAccess = '/data/access/1';", SOURCE)
        self.assertIn("const panelAccess = '/data/access/panel';", SOURCE)
        self.assertIn('PANEL_ACCESS_FILE_REUSED', SOURCE)
        self.assertIn("original.advanced_config.replace(panelBlock, '')", SOURCE)

    def test_secret_is_file_backed_and_never_logged(self):
        self.assertIn("passwordFile.startsWith('/run/')", SOURCE)
        self.assertIn('(passwordStat.mode & 0o077) === 0', SOURCE)
        self.assertIn("['passwd', '-apr1', '-stdin']", SOURCE)
        self.assertNotIn('console.log(password', SOURCE)
        self.assertNotIn('console.error(password', SOURCE)

    def test_preserves_existing_users_and_rejects_collision(self):
        self.assertIn("const sharedLines = shared.split('\\n').filter(Boolean);", SOURCE)
        self.assertIn('TEST_USERNAME_COLLISION', SOURCE)
        self.assertIn("sharedLines.join('\\n')", SOURCE)

    def test_preflights_and_has_scoped_rollback(self):
        self.assertIn("runNginx('-t', '-c', testConfig", SOURCE)
        self.assertIn('CONCURRENT_NPM_CHANGE', SOURCE)
        self.assertIn('panel.before.htpasswd', SOURCE)
        self.assertIn('host2.before.conf', SOURCE)
        self.assertIn('PANEL_ACCOUNT_RECOVERY_REQUIRED', SOURCE)


if __name__ == '__main__':
    unittest.main()
