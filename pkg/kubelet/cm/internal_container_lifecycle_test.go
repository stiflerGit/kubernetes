package cm

import (
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"testing"
)

func Test_writeCpuRtMultiRuntimeFile(t *testing.T) {
	type args struct {
		cgroupFs  string
		cpuSet    cpuset.CPUSet
		rtRuntime int64
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "nominal",
			args: args{
				cgroupFs:  "/sys/fs/cgroup/cpu,cpuacct/kubepods/burstable/podb2aab547-2e0d-413a-b0c6-81183b6cdb8c",
				cpuSet:    cpuset.NewCPUSet(1, 2, 3),
				rtRuntime: 10000000,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeCpuRtMultiRuntimeFile(tt.args.cgroupFs, tt.args.cpuSet, tt.args.rtRuntime); (err != nil) != tt.wantErr {
				t.Errorf("writeCpuRtMultiRuntimeFile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
