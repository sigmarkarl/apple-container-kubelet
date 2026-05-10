# vhs tape — produces docs/demo.gif
#
# Prereqs (one time):
#   brew install vhs
#   brew tap sigmarkarl/tap
#   brew install apple-container-kubelet
#   # have a kubeconfig pointing at a reachable cluster
#
# Render:
#   vhs scripts/demo.tape
#
# This tape assumes apple-container-kubelet is already installed and that
# `kubectl get nodes` will show a virtual node once the kubelet starts.
# The tape uses tmux-style sleeps so the GIF is readable, not real-time.

Output docs/demo.gif

Set FontSize 14
Set Width 1100
Set Height 640
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set PlaybackSpeed 1.0

Type "# Install"
Sleep 600ms
Enter
Type "brew install sigmarkarl/tap/apple-container-kubelet"
Sleep 800ms
Enter
Sleep 2s

Type "# Start the virtual kubelet (registers darwin/arm64 node)"
Sleep 600ms
Enter
Type "apple-container-kubelet &"
Sleep 800ms
Enter
Sleep 3s

Type "kubectl get nodes -l node.kubernetes.io/applevm=true"
Sleep 400ms
Enter
Sleep 3s

Type "# Schedule a pod onto it"
Sleep 600ms
Enter
Type "kubectl apply -f - <<'YAML'"
Enter
Type "apiVersion: v1"
Enter
Type "kind: Pod"
Enter
Type "metadata: { name: demo }"
Enter
Type "spec:"
Enter
Type "  nodeSelector: { node.kubernetes.io/applevm: \"true\" }"
Enter
Type "  tolerations: [{ key: virtual-kubelet.io/provider, value: applevm, effect: NoSchedule }]"
Enter
Type "  containers: [{ name: app, image: docker.io/library/alpine, command: [sleep, \"3600\"] }]"
Enter
Type "YAML"
Enter
Sleep 4s

Type "kubectl get pod demo -w"
Enter
Sleep 5s
Ctrl+C

Type "kubectl exec -it demo -- uname -a"
Enter
Sleep 3s
