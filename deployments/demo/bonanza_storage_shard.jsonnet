local statePath = std.extVar('STATE_PATH');
local replica = std.extVar('REPLICA');
local shard = std.extVar('SHARD');

{
  grpcServers: [{
    listenPaths: ['%s/bonanza_storage_shard_%s%s.sock' % [statePath, replica, shard]],
    authenticationPolicy: { allow: {} },
  }],

  leasesMapRecordsCount: 1e6,
  leasesMapLeaseCompletenessDuration: '120s',
  leasesMapMaximumGetAttempts: 16,
  leasesMapMaximumPutAttempts: 64,
}
