import unittest
from watchdog import decision


class WatchdogTests(unittest.TestCase):
    def test_startup_grace(self):
        self.assertFalse(decision({'failures': 5}, False, 1000, 950)[1])

    def test_three_failures(self):
        state = {}
        for now in (1000, 1030):
            state, restart = decision(state, False, now, 0)
            self.assertFalse(restart)
        self.assertTrue(decision(state, False, 1060, 0)[1])

    def test_recovery_resets_counter(self):
        state, restart = decision({'failures': 2}, True, 1000, 0)
        self.assertEqual(state['failures'], 0)
        self.assertFalse(restart)

    def test_cooldown_and_hourly_ceiling(self):
        self.assertFalse(decision({'failures': 5, 'last_restart': 900}, False, 1000, 0)[1])
        self.assertFalse(decision({'failures': 5, 'restarts': [100, 200, 300]}, False, 1000, 0)[1])
        self.assertTrue(decision({'failures': 5, 'restarts': [100, 200, 300]}, False, 5000, 0)[1])


if __name__ == '__main__':
    unittest.main()
