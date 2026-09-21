import unittest
from analyze import summary, analyze, paired_cluster_ci


class StatisticsTests(unittest.TestCase):
    def test_percentiles_and_empty(self):
        self.assertEqual(summary([1,2,3,4,5]), {'n':5,'mean':3,'p50':3,'p95':4.8})
        self.assertIsNone(summary([])['mean'])

    def test_failure_is_not_fast_success(self):
        case = {'id':'x','split':'test','group':'g','candidates':[], 'referenceDocumentIds':[]}
        rows = [{'phase':'test','arm':'jev','caseId':'x','repeat':1,'status':s,'durationMs':t}
                for s,t in [('ok',100),('error',20)]]
        result = analyze({'cases':[case]},rows,[],[])['latency']['jev']
        self.assertEqual(result['successRate'], .5)
        self.assertEqual(result['successfulMs']['n'],1)
        self.assertEqual(result['allAttemptMs']['n'],2)

    def test_pairs_exclude_missing_and_cluster(self):
        result = paired_cluster_ci({'a':.1,'b':.2,'c':.3},{'a':.2,'b':.3}, {'a':'g','b':'g','c':'h'},100)
        self.assertEqual(result['pairs'],2)
        self.assertEqual(result['clusters'],1)
        self.assertAlmostEqual(result['difference'],.1)
        self.assertAlmostEqual(result['leftMean'], .15)
        self.assertAlmostEqual(result['rightMean'], .25)

    def test_usage_matches_language_and_excludes_warmups(self):
        case = {'id':'en','split':'test','group':'g','candidates':[]}
        rows = [{'phase':phase,'arm':'jev','caseId':case_id,'repeat':1,
                 'status':'error','durationMs':1,'error':'test',
                 'calls':[{'costUSD':cost,'model':'jev'}]}
                for case_id,phase,cost in [('en','test',.01),('zh','test',.02),('en','warmup',.03)]]
        usage = analyze({'cases':[case]},rows,[],[])['jevUsage']
        self.assertAlmostEqual(usage['reportedCostUSD'], .01)
        self.assertEqual(usage['attemptedCalls'], 1)


if __name__ == '__main__':
    unittest.main()
