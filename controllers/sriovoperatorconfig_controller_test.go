package controllers

import (
	"context"
	"strings"
	"sync"

	admv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sriovnetworkv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	constants "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/consts"
	mock_platforms "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/platforms/mock"
	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/platforms/openshift"
	util "github.com/k8snetworkplumbingwg/sriov-network-operator/test/util"
)

var _ = Describe("SriovOperatorConfig controller", Ordered, func() {
	var cancel context.CancelFunc
	var ctx context.Context

	BeforeAll(func() {
		By("Create SriovOperatorConfig controller k8s objs")
		config := &sriovnetworkv1.SriovOperatorConfig{}
		config.SetNamespace(testNamespace)
		config.SetName(constants.DefaultConfigName)
		config.Spec = sriovnetworkv1.SriovOperatorConfigSpec{
			EnableInjector:           func() *bool { b := true; return &b }(),
			EnableOperatorWebhook:    func() *bool { b := true; return &b }(),
			ConfigDaemonNodeSelector: map[string]string{},
			LogLevel:                 2,
		}
		Expect(k8sClient.Create(context.Background(), config)).Should(Succeed())
		DeferCleanup(func() {
			err := k8sClient.Delete(context.Background(), config)
			Expect(err).ToNot(HaveOccurred())
		})

		// Create default SriovNetworkNodePolicy
		defaultPolicy := &sriovnetworkv1.SriovNetworkNodePolicy{}
		defaultPolicy.SetNamespace(testNamespace)
		defaultPolicy.SetName(constants.DefaultPolicyName)
		defaultPolicy.Spec = sriovnetworkv1.SriovNetworkNodePolicySpec{
			NumVfs:       0,
			NodeSelector: make(map[string]string),
			NicSelector:  sriovnetworkv1.SriovNetworkNicSelector{},
		}
		Expect(k8sClient.Create(context.Background(), defaultPolicy)).Should(Succeed())
		DeferCleanup(func() {
			err := k8sClient.Delete(context.Background(), defaultPolicy)
			Expect(err).ToNot(HaveOccurred())
		})

		// setup controller manager
		By("Setup controller manager")
		k8sManager, err := setupK8sManagerForTest()
		Expect(err).ToNot(HaveOccurred())

		t := GinkgoT()
		mockCtrl := gomock.NewController(t)
		platformHelper := mock_platforms.NewMockInterface(mockCtrl)
		platformHelper.EXPECT().GetFlavor().Return(openshift.OpenshiftFlavorDefault).AnyTimes()
		platformHelper.EXPECT().IsOpenshiftCluster().Return(false).AnyTimes()
		platformHelper.EXPECT().IsHypershift().Return(false).AnyTimes()

		err = (&SriovOperatorConfigReconciler{
			Client:         k8sManager.GetClient(),
			Scheme:         k8sManager.GetScheme(),
			PlatformHelper: platformHelper,
		}).SetupWithManager(k8sManager)
		Expect(err).ToNot(HaveOccurred())

		ctx, cancel = context.WithCancel(context.Background())

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			By("Start controller manager")
			err := k8sManager.Start(ctx)
			Expect(err).ToNot(HaveOccurred())
		}()

		DeferCleanup(func() {
			By("Shut down manager")
			cancel()
			wg.Wait()
		})
	})

	Context("When is up", func() {
		JustBeforeEach(func() {
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())
			config.Spec = sriovnetworkv1.SriovOperatorConfigSpec{
				EnableInjector:        func() *bool { b := true; return &b }(),
				EnableOperatorWebhook: func() *bool { b := true; return &b }(),
				// ConfigDaemonNodeSelector: map[string]string{},
				LogLevel: 2,
			}
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should have webhook enable", func() {
			mutateCfg := &admv1.MutatingWebhookConfiguration{}
			err := util.WaitForNamespacedObject(mutateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout*3)
			Expect(err).NotTo(HaveOccurred())

			validateCfg := &admv1.ValidatingWebhookConfiguration{}
			err = util.WaitForNamespacedObject(validateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout*3)
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("should have daemonset enabled by default",
			func(dsName string) {
				// wait for sriov-network-operator to be ready
				daemonSet := &appsv1.DaemonSet{}
				err := util.WaitForNamespacedObject(daemonSet, k8sClient, testNamespace, dsName, util.RetryInterval, util.APITimeout)
				Expect(err).NotTo(HaveOccurred())
			},
			Entry("operator-webhook", "operator-webhook"),
			Entry("network-resources-injector", "network-resources-injector"),
			Entry("sriov-network-config-daemon", "sriov-network-config-daemon"),
		)

		It("should be able to turn network-resources-injector on/off", func() {
			By("set disable to enableInjector")
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			*config.Spec.EnableInjector = false
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet := &appsv1.DaemonSet{}
			err = util.WaitForNamespacedObjectDeleted(daemonSet, k8sClient, testNamespace, "network-resources-injector", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			mutateCfg := &admv1.MutatingWebhookConfiguration{}
			err = util.WaitForNamespacedObjectDeleted(mutateCfg, k8sClient, testNamespace, "network-resources-injector-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			By("set enable to enableInjector")
			err = util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			*config.Spec.EnableInjector = true
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet = &appsv1.DaemonSet{}
			err = util.WaitForNamespacedObject(daemonSet, k8sClient, testNamespace, "network-resources-injector", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			mutateCfg = &admv1.MutatingWebhookConfiguration{}
			err = util.WaitForNamespacedObject(mutateCfg, k8sClient, testNamespace, "network-resources-injector-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should be able to turn operator-webhook on/off", func() {

			By("set disable to enableOperatorWebhook")
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			*config.Spec.EnableOperatorWebhook = false
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet := &appsv1.DaemonSet{}
			err = util.WaitForNamespacedObjectDeleted(daemonSet, k8sClient, testNamespace, "operator-webhook", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			mutateCfg := &admv1.MutatingWebhookConfiguration{}
			err = util.WaitForNamespacedObjectDeleted(mutateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			validateCfg := &admv1.ValidatingWebhookConfiguration{}
			err = util.WaitForNamespacedObjectDeleted(validateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			By("set disable to enableOperatorWebhook")
			err = util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			*config.Spec.EnableOperatorWebhook = true
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet = &appsv1.DaemonSet{}
			err = util.WaitForNamespacedObject(daemonSet, k8sClient, testNamespace, "operator-webhook", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			mutateCfg = &admv1.MutatingWebhookConfiguration{}
			err = util.WaitForNamespacedObject(mutateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			validateCfg = &admv1.ValidatingWebhookConfiguration{}
			err = util.WaitForNamespacedObject(validateCfg, k8sClient, testNamespace, "sriov-operator-webhook-config", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should be able to update the node selector of sriov-network-config-daemon", func() {
			By("specify the configDaemonNodeSelector")
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())
			config.Spec.ConfigDaemonNodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() map[string]string {
				// By("wait for DaemonSet NodeSelector")
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "sriov-network-config-daemon", Namespace: testNamespace}, daemonSet)
				if err != nil {
					return nil
				}
				return daemonSet.Spec.Template.Spec.NodeSelector
			}, util.APITimeout, util.RetryInterval).Should(Equal(config.Spec.ConfigDaemonNodeSelector))
		})

		It("should be able to do multiple updates to the node selector of sriov-network-config-daemon", func() {
			By("changing the configDaemonNodeSelector")
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())
			config.Spec.ConfigDaemonNodeSelector = map[string]string{"labelA": "", "labelB": "", "labelC": ""}
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			config.Spec.ConfigDaemonNodeSelector = map[string]string{"labelA": "", "labelB": ""}
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() map[string]string {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "sriov-network-config-daemon", Namespace: testNamespace}, daemonSet)
				if err != nil {
					return nil
				}
				return daemonSet.Spec.Template.Spec.NodeSelector
			}, util.APITimeout, util.RetryInterval).Should(Equal(config.Spec.ConfigDaemonNodeSelector))
		})

		It("should not render disable-plugins cmdline flag of sriov-network-config-daemon if disablePlugin not provided in spec", func() {
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() string {
				daemonSet := &appsv1.DaemonSet{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "sriov-network-config-daemon", Namespace: testNamespace}, daemonSet)
				if err != nil {
					return ""
				}
				return strings.Join(daemonSet.Spec.Template.Spec.Containers[0].Args, " ")
			}, util.APITimeout*10, util.RetryInterval).Should(And(Not(BeEmpty()), Not(ContainSubstring("disable-plugins"))))
		})

		It("should render disable-plugins cmdline flag of sriov-network-config-daemon if disablePlugin provided in spec", func() {
			config := &sriovnetworkv1.SriovOperatorConfig{}
			err := util.WaitForNamespacedObject(config, k8sClient, testNamespace, "default", util.RetryInterval, util.APITimeout)
			Expect(err).NotTo(HaveOccurred())

			config.Spec.DisablePlugins = sriovnetworkv1.PluginNameSlice{"mellanox"}
			err = k8sClient.Update(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() string {
				daemonSet := &appsv1.DaemonSet{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "sriov-network-config-daemon", Namespace: testNamespace}, daemonSet)
				if err != nil {
					return ""
				}
				return strings.Join(daemonSet.Spec.Template.Spec.Containers[0].Args, " ")
			}, util.APITimeout*10, util.RetryInterval).Should(ContainSubstring("disable-plugins=mellanox"))
		})
	})
})
